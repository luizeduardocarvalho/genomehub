// Package httpapi is the node-facing HTTP surface: a read-only, content-addressed
// API over the local segment store plus a catalog of manifests and deltas. The
// same handler serves an origin or, later, a peer — a client cannot tell the
// difference, because every segment is fetched by hash and re-verified by the
// receiver. Transport is plain HTTP/1.1 for now; the routes are transport-
// agnostic, so HTTP/2 (h2c) or QUIC is a later swap behind the same paths.
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/delta"
	"github.com/luizeduardocarvalho/genomehub/internal/events"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
)

// Catalog maps assembly names to the manifest / delta files that describe them.
type Catalog struct {
	Manifests map[string]string // assembly -> manifest path
	Deltas    map[string]string // assembly -> raw delta (.ghd) path
	Recipes   map[string]string // assembly -> delta recipe (chunked) path
}

// ScanCatalog builds a Catalog by reading every *.manifest.json and
// *.delta.{ghd,json} in dir and indexing them by their declared assembly name.
func ScanCatalog(dir string) (*Catalog, error) {
	c := &Catalog{Manifests: map[string]string{}, Deltas: map[string]string{}, Recipes: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		p := filepath.Join(dir, name)
		switch {
		case strings.HasSuffix(name, ".deltarecipe.json"):
			r, err := delta.ReadRecipe(p)
			if err == nil && r.Assembly != "" {
				c.Recipes[r.Assembly] = p
			}
		case strings.HasSuffix(name, ".manifest.json"):
			m, err := manifest.Read(p)
			if err == nil && m.Assembly != "" {
				c.Manifests[m.Assembly] = p
			}
		case strings.HasSuffix(name, ".delta.ghd"), strings.HasSuffix(name, ".delta.json"):
			d, err := delta.Read(p)
			if err == nil && d.Assembly != "" {
				c.Deltas[d.Assembly] = p
			}
		}
	}
	return c, nil
}

// Seeding is per-assembly coverage: how much of a genome this node can actually
// serve. For a manifest genome it is held∩manifest segments over total; a node
// with Have==Total is a full seed, a partial node is a useful but incomplete
// cache. Deltas/recipes are file-served, reported with Total==0 (n/a bar).
type Seeding struct {
	Assembly string `json:"assembly"`
	Kind     string `json:"kind"` // "manifest" | "delta"
	Have     int    `json:"have"`
	Total    int    `json:"total"`
}

// ServeHit is one recently served request, for the participant's "I am a source"
// activity feed. Assembly is attributed when the path or segment hash maps to one.
type ServeHit struct {
	Path     string `json:"path"`
	Assembly string `json:"assembly,omitempty"`
	Bytes    int    `json:"bytes"`
	Status   int    `json:"status"`
	AgoSec   int    `json:"ago_seconds"`
}

// Status is the node's self-report, consumed by the status/top/control TUIs.
type Status struct {
	UptimeSeconds int      `json:"uptime_seconds"`
	SegmentsHeld  int      `json:"segments_held"`
	Manifests     []string `json:"manifests"`
	Deltas        []string `json:"deltas"`
	Requests      int64    `json:"requests"`
	BytesServed   int64    `json:"bytes_served"`

	// Participant view (Tier 1–3).
	Seeding     []Seeding      `json:"seeding"`       // per-genome coverage
	ReqPerSec   float64        `json:"req_per_sec"`   // serving rate, last 10s
	BytesPerSec float64        `json:"bytes_per_sec"` // upload rate, last 10s
	Served      []ServeHit     `json:"served"`        // last requests served
	Recent      []events.Event `json:"recent"`        // last imports/downloads (event log)
}

// hit is one served request retained for rate + feed computation.
type hit struct {
	t        time.Time
	bytes    int
	status   int
	path     string
	assembly string
}

const (
	rateWindow  = 10 * time.Second // sliding window for req/s + bytes/s
	feedSize    = 16               // last-N served requests kept for display
	recentLimit = 8                // last-N import/download events surfaced
)

// server holds the node's serving state and live counters.
type server struct {
	store      *store.Store
	cat        *Catalog
	start      time.Time
	eventsPath string
	reqs       atomic.Int64
	bytes      atomic.Int64

	mu       sync.Mutex
	rateWin  []hit             // requests within rateWindow (pruned)
	feed     []hit             // last feedSize requests (ring)
	segOf    map[string]string // segment hash (no prefix) -> assembly
	covCache map[string]segset // assembly -> its manifest segment hash set
}

// segset is a manifest's segment-hash set plus its total, cached so coverage is
// recomputed each poll without re-parsing the manifest.
type segset struct {
	hashes map[string]struct{}
	total  int
}

// NewHandler builds the HTTP handler for a node. eventsPath is the local activity
// log to surface in /status (may be ""); manifests are lazily parsed for coverage.
func NewHandler(s *store.Store, cat *Catalog, eventsPath string) http.Handler {
	srv := &server{store: s, cat: cat, start: time.Now(), eventsPath: eventsPath}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		heldSet := map[string]struct{}{}
		if hs, err := s.ListHashes(); err == nil {
			for _, h := range hs {
				heldSet[strings.TrimPrefix(h, "blake3:")] = struct{}{}
			}
		}
		reqps, bps, feed := srv.activity()
		st := Status{
			UptimeSeconds: int(time.Since(srv.start).Seconds()),
			SegmentsHeld:  len(heldSet),
			Manifests:     keys(cat.Manifests),
			Deltas:        keys(cat.Deltas),
			Requests:      srv.reqs.Load(),
			BytesServed:   srv.bytes.Load(),
			Seeding:       srv.seeding(heldSet),
			ReqPerSec:     reqps,
			BytesPerSec:   bps,
			Served:        feed,
			Recent:        events.Tail(srv.eventsPath, recentLimit),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})

	// Raw, content-addressed segment bytes. The client re-hashes to verify.
	mux.HandleFunc("GET /segments/{hash}", func(w http.ResponseWriter, r *http.Request) {
		hash := strings.TrimPrefix(r.PathValue("hash"), "blake3:")
		data, err := s.Get(hash)
		if err != nil {
			http.Error(w, "segment not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	})

	mux.HandleFunc("GET /genomes/{assembly}/manifest", func(w http.ResponseWriter, r *http.Request) {
		p, ok := cat.Manifests[r.PathValue("assembly")]
		if !ok {
			http.Error(w, "no manifest for assembly", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		http.ServeFile(w, r, p)
	})

	mux.HandleFunc("GET /deltas/{assembly}", func(w http.ResponseWriter, r *http.Request) {
		a := r.PathValue("assembly")
		// Prefer the chunked recipe (swarms across peers); fall back to the raw
		// blob. The client distinguishes by content: a recipe is JSON, a raw
		// delta starts with the "GHD1" magic.
		if p, ok := cat.Recipes[a]; ok {
			w.Header().Set("Content-Type", "application/json")
			http.ServeFile(w, r, p)
			return
		}
		if p, ok := cat.Deltas[a]; ok {
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, p)
			return
		}
		http.Error(w, "no delta for assembly", http.StatusNotFound)
	})

	// Human/debug view of what this node can serve.
	mux.HandleFunc("GET /catalog", func(w http.ResponseWriter, _ *http.Request) {
		type entry struct {
			Assembly string `json:"assembly"`
			Kind     string `json:"kind"`
		}
		var list []entry
		for a := range cat.Manifests {
			list = append(list, entry{a, "manifest"})
		}
		for a := range cat.Recipes {
			list = append(list, entry{a, "delta-recipe"})
		}
		for a := range cat.Deltas {
			if _, chunked := cat.Recipes[a]; chunked {
				continue // recipe already listed; don't double-count
			}
			list = append(list, entry{a, "delta"})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Assembly < list[j].Assembly })
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	})

	return srv.logging(mux)
}

// logging records each request's method, path, status and bytes written (for the
// server/client boundary log) and updates the node's live counters + activity feed.
func (srv *server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(rec, r)
		srv.reqs.Add(1)
		srv.bytes.Add(int64(rec.bytes))
		// /status and /healthz are observers polling us — don't count them as
		// "serving data", or the activity feed is all self-poll noise.
		if p := r.URL.Path; p != "/status" && p != "/healthz" {
			srv.record(hit{t: time.Now(), bytes: rec.bytes, status: rec.status,
				path: p, assembly: srv.assemblyOf(p)})
		}
		fmt.Printf("%s %s -> %d  %s  %s\n", r.Method, r.URL.Path, rec.status,
			humanBytes(rec.bytes), time.Since(start).Round(time.Millisecond))
	})
}

// record appends a served request to the sliding rate window and the display
// feed, pruning the window to rateWindow.
func (srv *server) record(h hit) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	cut := time.Now().Add(-rateWindow)
	srv.rateWin = append(srv.rateWin, h)
	for len(srv.rateWin) > 0 && srv.rateWin[0].t.Before(cut) {
		srv.rateWin = srv.rateWin[1:]
	}
	srv.feed = append(srv.feed, h)
	if len(srv.feed) > feedSize {
		srv.feed = srv.feed[len(srv.feed)-feedSize:]
	}
}

// activity returns req/s and bytes/s over the last rateWindow plus the recent
// served feed (newest first).
func (srv *server) activity() (reqps, bps float64, feed []ServeHit) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	cut := time.Now().Add(-rateWindow)
	var n, b int
	for _, h := range srv.rateWin {
		if h.t.Before(cut) {
			continue
		}
		n++
		b += h.bytes
	}
	secs := rateWindow.Seconds()
	reqps, bps = float64(n)/secs, float64(b)/secs
	feed = make([]ServeHit, 0, len(srv.feed))
	now := time.Now()
	for i := len(srv.feed) - 1; i >= 0; i-- {
		h := srv.feed[i]
		feed = append(feed, ServeHit{
			Path: h.path, Assembly: h.assembly, Bytes: h.bytes,
			Status: h.status, AgoSec: int(now.Sub(h.t).Seconds()),
		})
	}
	return reqps, bps, feed
}

// seeding reports per-manifest coverage against the held segment set, plus a
// flat entry per delta/recipe (file-served, no bar). Manifest segment sets are
// parsed once and cached; coverage is a set-intersection each poll.
func (srv *server) seeding(held map[string]struct{}) []Seeding {
	out := make([]Seeding, 0, len(srv.cat.Manifests)+len(srv.cat.Deltas))
	for _, a := range keys(srv.cat.Manifests) {
		ss := srv.segsFor(a)
		have := 0
		for h := range ss.hashes {
			if _, ok := held[h]; ok {
				have++
			}
		}
		out = append(out, Seeding{Assembly: a, Kind: "manifest", Have: have, Total: ss.total})
	}
	for _, a := range keys(srv.cat.Deltas) {
		out = append(out, Seeding{Assembly: a, Kind: "delta"})
	}
	for _, a := range keys(srv.cat.Recipes) {
		out = append(out, Seeding{Assembly: a, Kind: "delta"})
	}
	return out
}

// segsFor returns (and lazily builds) the cached segment-hash set for a manifest
// assembly, also populating the hash->assembly reverse index for attribution.
func (srv *server) segsFor(assembly string) segset {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.covCache == nil {
		srv.covCache = map[string]segset{}
		srv.segOf = map[string]string{}
	}
	if ss, ok := srv.covCache[assembly]; ok {
		return ss
	}
	ss := segset{hashes: map[string]struct{}{}}
	if p, ok := srv.cat.Manifests[assembly]; ok {
		if m, err := manifest.Read(p); err == nil {
			for _, c := range m.Chromosomes {
				for _, seg := range c.Segments {
					h := strings.TrimPrefix(seg.Hash, "blake3:")
					ss.hashes[h] = struct{}{}
					srv.segOf[h] = assembly
				}
			}
			ss.total = len(ss.hashes)
		}
	}
	srv.covCache[assembly] = ss
	return ss
}

// assemblyOf attributes a request path to an assembly: direct for /deltas/{a}
// and /genomes/{a}/manifest, reverse-indexed for /segments/{hash}.
func (srv *server) assemblyOf(path string) string {
	switch {
	case strings.HasPrefix(path, "/deltas/"):
		return strings.TrimPrefix(path, "/deltas/")
	case strings.HasPrefix(path, "/genomes/"):
		rest := strings.TrimPrefix(path, "/genomes/")
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return rest[:i]
		}
		return rest
	case strings.HasPrefix(path, "/segments/"):
		h := strings.TrimPrefix(strings.TrimPrefix(path, "/segments/"), "blake3:")
		srv.ensureSegIndex()
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.segOf[h]
	}
	return ""
}

// ensureSegIndex makes sure every manifest has been parsed at least once so the
// hash->assembly index is populated before a segment lookup.
func (srv *server) ensureSegIndex() {
	for _, a := range keys(srv.cat.Manifests) {
		srv.segsFor(a)
	}
}

// keys returns the sorted keys of a map.
func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func humanBytes(b int) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}
