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
	"io"
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
	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
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
	Organism string `json:"organism,omitempty"` // species, from the manifest
	Kind     string `json:"kind"`               // "manifest" | "delta"
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
	Seeding     []Seeding        `json:"seeding"`       // per-genome coverage
	ReqPerSec   float64          `json:"req_per_sec"`   // serving rate, last 10s
	BytesPerSec float64          `json:"bytes_per_sec"` // upload rate, last 10s
	Served      []ServeHit       `json:"served"`        // last requests served
	Recent      []events.Event   `json:"recent"`        // last imports/downloads (event log)
	Downloads   []DownloadStatus `json:"downloads"`     // in-node downloads in progress
}

// RegistryEntry is a genome summary for browsing — no segment hashes, so it is
// cheap to list even for huge genomes. Served by /registry.
type RegistryEntry struct {
	Assembly string `json:"assembly"`
	Organism string `json:"organism,omitempty"`
	Version  int    `json:"version"`
	Segments int    `json:"segments"`
	Bases    int    `json:"bases"`
	Kind     string `json:"kind"` // "manifest" | "delta"
}

// DiscoverEntry is a registry genome plus this node's coverage of it — the basis
// of the TUI Discover tab ("ATHENA2 exists, you already hold 60%"). Served by
// /discover, which pulls the upstream registry and intersects with the store.
type DiscoverEntry struct {
	RegistryEntry
	Have          int  `json:"have"`
	Total         int  `json:"total"`
	Local         bool `json:"local"`          // already in this node's catalog
	Sources       int  `json:"sources"`        // how many nodes' registries list it (availability)
	CoverageKnown bool `json:"coverage_known"` // false = too big to compute eagerly; fetch /coverage/{a} on demand
}

// maxEagerSegments bounds how big a genome's manifest may be before discover
// skips computing exact coverage for it (a huge manifest — e.g. CVI0 at ~349 MB
// — would stall a browse). Coverage for bigger genomes is computed on demand via
// GET /coverage/{assembly} when the row is selected.
const maxEagerSegments = 20000

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
	store       *store.Store
	catalog     atomic.Pointer[Catalog] // swapped copy-on-write when a manifest is tracked/updated
	start       time.Time
	eventsPath  string
	manifestDir string // where tracked/updated manifests are persisted
	registryURL string // upstream registry (origin) for the discover endpoint
	trackerURL  string // tracker for peer-routed segment fetch (may be "")
	reqs        atomic.Int64
	bytes       atomic.Int64

	mu        sync.Mutex
	rateWin   []hit             // requests within rateWindow (pruned)
	feed      []hit             // last feedSize requests (ring)
	segOf     map[string]string // segment hash (no prefix) -> assembly
	covCache  map[string]segset // local assembly -> its manifest segment hash set
	discCache map[string]segset // remote (registry) assembly -> hash set, for discover coverage

	dlMu    sync.Mutex
	dlTasks map[string]*dlTask // assembly -> download task (running or finished)
}

// segset is a manifest's segment-hash set plus its total, cached so coverage is
// recomputed each poll without re-parsing the manifest.
type segset struct {
	hashes   map[string]struct{}
	total    int
	organism string
	version  int
	bases    int
}

// NewHandler builds the HTTP handler for a node. eventsPath is the local activity
// log to surface in /status (may be ""); registryURL is the upstream registry
// (origin) the discover/track endpoints pull from (may be ""); manifestDir is
// where tracked manifests are persisted (may be ""); manifests are lazily parsed
// for coverage.
func NewHandler(s *store.Store, cat *Catalog, eventsPath, registryURL, manifestDir, trackerURL string) http.Handler {
	srv := &server{start: time.Now(), store: s, eventsPath: eventsPath,
		manifestDir: manifestDir, registryURL: strings.TrimRight(registryURL, "/"),
		trackerURL: strings.TrimRight(trackerURL, "/")}
	srv.catalog.Store(cat)
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
			Manifests:     keys(srv.cur().Manifests),
			Deltas:        keys(srv.cur().Deltas),
			Requests:      srv.reqs.Load(),
			BytesServed:   srv.bytes.Load(),
			Seeding:       srv.seeding(heldSet),
			ReqPerSec:     reqps,
			BytesPerSec:   bps,
			Served:        feed,
			Recent:        events.Tail(srv.eventsPath, recentLimit),
			Downloads:     srv.downloads(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})

	// Browsable summary of every genome this node knows (its catalog). Origin's
	// is the network registry; a peer's lists what it can describe.
	mux.HandleFunc("GET /registry", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, srv.registry())
	})

	// Discover: the upstream registry's genomes, each annotated with how much of
	// it this node already holds — so a partial seeder learns "you have 60% of
	// ATHENA2" for a genome it never imported.
	mux.HandleFunc("GET /discover", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, srv.discover())
	})

	// On-demand exact coverage for one genome — used by the TUI when a big genome
	// (whose coverage discover deferred) is selected.
	mux.HandleFunc("GET /coverage/{assembly}", func(w http.ResponseWriter, r *http.Request) {
		a := r.PathValue("assembly")
		held := map[string]struct{}{}
		if hs, err := s.ListHashes(); err == nil {
			for _, h := range hs {
				held[strings.TrimPrefix(h, "blake3:")] = struct{}{}
			}
		}
		_, local := srv.cur().Manifests[a]
		have, total := srv.coverageOf(a, local, held, srv.registryBases())
		writeJSON(w, DiscoverEntry{
			RegistryEntry: RegistryEntry{Assembly: a, Segments: total},
			Have:          have, Total: total, Local: local, CoverageKnown: true,
		})
	})

	// Track / update a genome's manifest: fetch it from the registry, persist it,
	// and start serving + reporting coverage for it. Lock-free (a manifest is a
	// file), so it works while the node is live. The TUI's [T]/[U] keys hit this.
	mux.HandleFunc("POST /actions/manifest", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Assembly string `json:"assembly"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Assembly == "" {
			http.Error(w, "bad request: need {assembly}", http.StatusBadRequest)
			return
		}
		ver, err := srv.trackManifest(req.Assembly)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"assembly": req.Assembly, "version": ver, "tracked": true})
	})

	// Download the missing segments of a genome into the local store, as a
	// cancellable in-node task (the node owns the store lock, so this is the only
	// process that can fetch while serving). pause/resume/cancel control it; live
	// progress shows up in /status.Downloads.
	dlAction := func(op string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Assembly string   `json:"assembly"`
				Chroms   []string `json:"chroms,omitempty"`
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Assembly == "" {
				http.Error(w, "bad request: need {assembly}", http.StatusBadRequest)
				return
			}
			switch op {
			case "start":
				if err := srv.startDownload(req.Assembly, req.Chroms); err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
			case "pause":
				srv.pauseDownload(req.Assembly)
			case "resume":
				srv.resumeDownload(req.Assembly)
			case "cancel":
				srv.cancelDownload(req.Assembly)
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	// Per-chromosome coverage for the download picker.
	mux.HandleFunc("GET /genomes/{assembly}/chromosomes", func(w http.ResponseWriter, r *http.Request) {
		cov, err := srv.chromCoverage(r.PathValue("assembly"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, cov)
	})

	mux.HandleFunc("POST /actions/download", dlAction("start"))
	mux.HandleFunc("POST /actions/download/pause", dlAction("pause"))
	mux.HandleFunc("POST /actions/download/resume", dlAction("resume"))
	mux.HandleFunc("POST /actions/download/cancel", dlAction("cancel"))

	// Delete a genome from the local cache: free segments no other held genome
	// needs, and stop seeding it.
	mux.HandleFunc("POST /actions/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Assembly string `json:"assembly"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Assembly == "" {
			http.Error(w, "bad request: need {assembly}", http.StatusBadRequest)
			return
		}
		res, err := srv.deleteGenome(req.Assembly)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, res)
	})

	// Stop seeding a genome (remove its manifest) without deleting its segments.
	mux.HandleFunc("POST /actions/unseed", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Assembly string `json:"assembly"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Assembly == "" {
			http.Error(w, "bad request: need {assembly}", http.StatusBadRequest)
			return
		}
		if !srv.unseed(req.Assembly) {
			http.Error(w, "not in catalog", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Reconstruct a held genome to a FASTA on the node's filesystem, verifying
	// integrity (chromosome hashes; for a delta, the reference + query hashes).
	mux.HandleFunc("POST /actions/reconstruct", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Assembly string `json:"assembly"`
			Output   string `json:"output,omitempty"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Assembly == "" {
			http.Error(w, "bad request: need {assembly}", http.StatusBadRequest)
			return
		}
		chroms, err := srv.reconstructGenome(req.Assembly)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		path, err := srv.reconstructPath(req.Assembly, req.Output)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := fasta.Write(path, chroms); err != nil {
			http.Error(w, "write fasta: "+err.Error(), http.StatusInternalServerError)
			return
		}
		bases := 0
		for _, c := range chroms {
			bases += len(c.Sequence)
		}
		fmt.Fprintf(os.Stdout, "reconstructed %s → %s (%d chroms, %d bp, verified)\n", req.Assembly, path, len(chroms), bases)
		writeJSON(w, ReconstructResult{Assembly: req.Assembly, Path: path, Chromosomes: len(chroms), Bases: bases, Verified: true})
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
		p, ok := srv.cur().Manifests[r.PathValue("assembly")]
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
		if p, ok := srv.cur().Recipes[a]; ok {
			w.Header().Set("Content-Type", "application/json")
			http.ServeFile(w, r, p)
			return
		}
		if p, ok := srv.cur().Deltas[a]; ok {
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
		for a := range srv.cur().Manifests {
			list = append(list, entry{a, "manifest"})
		}
		for a := range srv.cur().Recipes {
			list = append(list, entry{a, "delta-recipe"})
		}
		for a := range srv.cur().Deltas {
			if _, chunked := srv.cur().Recipes[a]; chunked {
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
	out := make([]Seeding, 0, len(srv.cur().Manifests)+len(srv.cur().Deltas))
	for _, a := range keys(srv.cur().Manifests) {
		ss := srv.segsFor(a)
		have := 0
		for h := range ss.hashes {
			if _, ok := held[h]; ok {
				have++
			}
		}
		out = append(out, Seeding{Assembly: a, Organism: ss.organism, Kind: "manifest", Have: have, Total: ss.total})
	}
	// Recipe-backed deltas: coverage = chunks held / total chunks.
	// Raw delta blobs: file-served from disk, always Have=1/Total=1 once tracked.
	seen := map[string]struct{}{}
	for _, a := range keys(srv.cur().Recipes) {
		rss := srv.recipeSegsFor(a)
		have := 0
		for h := range rss.hashes {
			if _, ok := held[h]; ok {
				have++
			}
		}
		out = append(out, Seeding{Assembly: a, Kind: "delta", Have: have, Total: rss.total})
		seen[a] = struct{}{}
	}
	for _, a := range keys(srv.cur().Deltas) {
		if _, dup := seen[a]; dup {
			continue
		}
		out = append(out, Seeding{Assembly: a, Kind: "delta", Have: 1, Total: 1})
	}
	return out
}

// cur returns the live catalog (swapped copy-on-write when a manifest is
// tracked or updated).
func (srv *server) cur() *Catalog { return srv.catalog.Load() }

// trackManifest fetches assembly's manifest from the upstream registry, persists
// it to manifestDir, and splices it into a fresh catalog so the node serves it
// and reports its coverage — all without touching the store lock (a manifest is
// a file, not a store entry). Used by both "track" (first time) and "update"
// (refresh to a newer version). Returns the manifest version.
func (srv *server) trackManifest(assembly string) (int, error) {
	if srv.registryURL == "" {
		return 0, fmt.Errorf("no upstream registry configured")
	}
	if srv.manifestDir == "" {
		return 0, fmt.Errorf("no manifest directory configured")
	}
	var m manifest.Manifest
	if err := getRemoteJSON(srv.registryURL+"/genomes/"+assembly+"/manifest", &m); err != nil {
		return 0, fmt.Errorf("fetch manifest: %w", err)
	}
	if m.Assembly == "" {
		return 0, fmt.Errorf("registry returned an empty manifest for %q", assembly)
	}
	if err := os.MkdirAll(srv.manifestDir, 0o755); err != nil {
		return 0, err
	}
	path := filepath.Join(srv.manifestDir, m.Assembly+".manifest.json")
	if err := m.Write(path); err != nil {
		return 0, fmt.Errorf("persist manifest: %w", err)
	}

	// Splice into a fresh catalog (copy-on-write) so concurrent readers are safe.
	old := srv.cur()
	next := &Catalog{
		Manifests: cloneMap(old.Manifests),
		Deltas:    cloneMap(old.Deltas),
		Recipes:   cloneMap(old.Recipes),
	}
	next.Manifests[m.Assembly] = path
	srv.catalog.Store(next)

	// Invalidate cached coverage so it is recomputed against the new manifest
	// (e.g. an update that added segments drops a full seed to partial).
	srv.mu.Lock()
	delete(srv.covCache, m.Assembly)
	delete(srv.discCache, m.Assembly)
	srv.mu.Unlock()
	return m.Version, nil
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func isGHD1(b []byte) bool {
	return len(b) >= 4 && string(b[:4]) == "GHD1"
}

// trackDelta fetches assembly's delta (recipe or raw blob) from the upstream
// registry, persists it to manifestDir, splices it into the catalog, and
// returns the reference assembly name. Idempotent — safe to call on re-download.
func (srv *server) trackDelta(assembly string) (string, error) {
	if srv.registryURL == "" {
		return "", fmt.Errorf("no upstream registry configured")
	}
	if srv.manifestDir == "" {
		return "", fmt.Errorf("no manifest directory configured")
	}
	resp, err := httpClient.Get(srv.registryURL + "/deltas/" + assembly)
	if err != nil {
		return "", fmt.Errorf("fetch delta: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s: no delta on registry", assembly)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /deltas/%s: status %d", assembly, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read delta: %w", err)
	}
	if err := os.MkdirAll(srv.manifestDir, 0o755); err != nil {
		return "", err
	}

	old := srv.cur()
	next := &Catalog{
		Manifests: cloneMap(old.Manifests),
		Deltas:    cloneMap(old.Deltas),
		Recipes:   cloneMap(old.Recipes),
	}
	var refAssembly string
	if isGHD1(body) {
		path := filepath.Join(srv.manifestDir, assembly+".delta.ghd")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return "", fmt.Errorf("persist delta: %w", err)
		}
		d, err := delta.Read(path)
		if err != nil {
			return "", fmt.Errorf("parse delta: %w", err)
		}
		refAssembly = d.Reference
		next.Deltas[assembly] = path
	} else {
		var r delta.Recipe
		if err := json.Unmarshal(body, &r); err != nil {
			return "", fmt.Errorf("parse delta recipe: %w", err)
		}
		refAssembly = r.Reference
		path := filepath.Join(srv.manifestDir, assembly+".deltarecipe.json")
		if err := r.Write(path); err != nil {
			return "", fmt.Errorf("persist recipe: %w", err)
		}
		next.Recipes[assembly] = path
	}
	srv.catalog.Store(next)
	srv.mu.Lock()
	delete(srv.covCache, assembly)
	delete(srv.discCache, assembly)
	srv.mu.Unlock()
	return refAssembly, nil
}

// localRefAssembly reads the reference assembly name from a locally cataloged
// delta (recipe or raw blob). Returns "" if the assembly is not a local delta.
func (srv *server) localRefAssembly(assembly string) string {
	cur := srv.cur()
	if p, ok := cur.Recipes[assembly]; ok {
		if r, err := delta.ReadRecipe(p); err == nil {
			return r.Reference
		}
	}
	if p, ok := cur.Deltas[assembly]; ok {
		if d, err := delta.Read(p); err == nil {
			return d.Reference
		}
	}
	return ""
}

// recipeChunkHashes returns the chunk-hash set for a recipe-backed delta. The
// caller uses it to partition have vs missing, exactly like chromHashes does for
// manifest genomes.
func (srv *server) recipeChunkHashes(assembly string) (map[string]struct{}, error) {
	p, ok := srv.cur().Recipes[assembly]
	if !ok {
		return nil, fmt.Errorf("%s: not a recipe-backed delta in catalog", assembly)
	}
	r, err := delta.ReadRecipe(p)
	if err != nil {
		return nil, fmt.Errorf("load recipe: %w", err)
	}
	out := map[string]struct{}{}
	for _, c := range r.Chunks {
		out[strings.TrimPrefix(c.Hash, "blake3:")] = struct{}{}
	}
	return out, nil
}

// recipeSegsFor returns (and lazily caches) the chunk-hash set for a
// recipe-backed delta, analogous to segsFor for manifest genomes.
func (srv *server) recipeSegsFor(assembly string) segset {
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
	if p, ok := srv.cur().Recipes[assembly]; ok {
		if r, err := delta.ReadRecipe(p); err == nil {
			for _, c := range r.Chunks {
				h := strings.TrimPrefix(c.Hash, "blake3:")
				ss.hashes[h] = struct{}{}
				srv.segOf[h] = assembly
			}
			ss.total = len(ss.hashes)
		}
	}
	srv.covCache[assembly] = ss
	return ss
}

// registry lists genome summaries for every genome in this node's catalog —
// the browsable index. Cheap: no segment hashes, just counts and metadata.
func (srv *server) registry() []RegistryEntry {
	cur := srv.cur()
	out := make([]RegistryEntry, 0, len(cur.Manifests)+len(cur.Recipes)+len(cur.Deltas))
	for _, a := range keys(cur.Manifests) {
		ss := srv.segsFor(a)
		out = append(out, RegistryEntry{
			Assembly: a, Organism: ss.organism, Version: ss.version,
			Segments: ss.total, Bases: ss.bases, Kind: "manifest",
		})
	}
	listed := map[string]struct{}{}
	for _, a := range keys(cur.Recipes) {
		r, err := delta.ReadRecipe(cur.Recipes[a])
		if err != nil {
			continue
		}
		out = append(out, RegistryEntry{Assembly: a, Kind: "delta", Segments: len(r.Chunks)})
		listed[a] = struct{}{}
	}
	for _, a := range keys(cur.Deltas) {
		if _, dup := listed[a]; dup {
			continue
		}
		out = append(out, RegistryEntry{Assembly: a, Kind: "delta", Segments: 1})
	}
	return out
}

// discover aggregates the registries of every node the tracker knows (plus the
// configured origin) into one genome list, annotated with this node's coverage
// of each and how many sources hold it. Discovery therefore survives origin going
// down and surfaces genomes that only peers hold — not just one upstream.
func (srv *server) discover() []DiscoverEntry {
	held := map[string]struct{}{}
	if hs, err := srv.store.ListHashes(); err == nil {
		for _, h := range hs {
			held[strings.TrimPrefix(h, "blake3:")] = struct{}{}
		}
	}

	// Union every reachable registry: keep the newest version, count sources.
	union := map[string]RegistryEntry{}
	sources := map[string]int{}
	add := func(reg []RegistryEntry) {
		for _, e := range reg {
			sources[e.Assembly]++
			if ex, ok := union[e.Assembly]; !ok || e.Version > ex.Version {
				union[e.Assembly] = e
			}
		}
	}
	bases := srv.registryBases()
	got := false
	for _, b := range bases {
		var reg []RegistryEntry
		if getRemoteJSON(b+"/registry", &reg) == nil {
			add(reg)
			got = true
		}
	}
	if !got { // tracker + origin all unreachable: at least describe ourselves
		add(srv.registry())
	}

	out := make([]DiscoverEntry, 0, len(union))
	for a, e := range union {
		_, local := srv.cur().Manifests[a]
		entry := DiscoverEntry{RegistryEntry: e, Local: local, Sources: sources[a]}
		// Exact coverage for genomes we already hold (free) or small enough to
		// fetch cheaply; defer the rest so a giant manifest never stalls a browse.
		if local || e.Segments <= maxEagerSegments {
			entry.Have, entry.Total = srv.coverageOf(a, local, held, bases)
			entry.CoverageKnown = true
		} else {
			entry.Total = e.Segments
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Assembly < out[j].Assembly })
	return out
}

// coverageOf returns this node's (have, total) segment coverage of a genome,
// fetching+caching its manifest hashes as needed.
func (srv *server) coverageOf(assembly string, local bool, held map[string]struct{}, bases []string) (have, total int) {
	hashes := srv.discoverHashes(assembly, local, bases)
	for h := range hashes {
		if _, ok := held[h]; ok {
			have++
		}
	}
	total = len(hashes)
	return have, total
}

// registryBases lists the registry/manifest base URLs to consult: every online
// node the tracker knows, plus the configured origin (deduped).
func (srv *server) registryBases() []string {
	seen := map[string]bool{}
	var out []string
	addBase := func(u string) {
		u = strings.TrimRight(u, "/")
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	if srv.trackerURL != "" {
		var nodes []struct {
			Address string `json:"address"`
			Online  bool   `json:"online"`
		}
		if getRemoteJSON(srv.trackerURL+"/nodes", &nodes) == nil {
			for _, n := range nodes {
				if n.Online {
					addBase(n.Address)
				}
			}
		}
	}
	addBase(srv.registryURL)
	return out
}

// discoverHashes returns the hash set for an assembly: chunk hashes for
// recipe-backed deltas, segment hashes for manifest genomes. Local catalog
// entries are free (cached); remote entries are fetched once then cached.
// For delta genomes the remote fetch tries /deltas/{a} if /manifest 404s.
func (srv *server) discoverHashes(assembly string, local bool, bases []string) map[string]struct{} {
	if local {
		cur := srv.cur()
		if _, ok := cur.Manifests[assembly]; ok {
			return srv.segsFor(assembly).hashes
		}
		return srv.recipeSegsFor(assembly).hashes
	}
	srv.mu.Lock()
	if srv.discCache == nil {
		srv.discCache = map[string]segset{}
	}
	if ss, ok := srv.discCache[assembly]; ok {
		srv.mu.Unlock()
		return ss.hashes
	}
	srv.mu.Unlock()

	ss := segset{hashes: map[string]struct{}{}}
	for _, b := range bases {
		var m manifest.Manifest
		if err := getRemoteJSON(b+"/genomes/"+assembly+"/manifest", &m); err == nil && m.Assembly != "" {
			for _, c := range m.Chromosomes {
				for _, seg := range c.Segments {
					ss.hashes[strings.TrimPrefix(seg.Hash, "blake3:")] = struct{}{}
				}
			}
			break
		}
		// Try recipe (delta genome): fetch /deltas/{a} and parse as recipe JSON.
		resp, err := httpClient.Get(b + "/deltas/" + assembly)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || isGHD1(body) {
			continue // raw blob has no granular hashes
		}
		var r delta.Recipe
		if json.Unmarshal(body, &r) == nil && len(r.Chunks) > 0 {
			for _, c := range r.Chunks {
				ss.hashes[strings.TrimPrefix(c.Hash, "blake3:")] = struct{}{}
			}
			break
		}
	}
	srv.mu.Lock()
	srv.discCache[assembly] = ss
	srv.mu.Unlock()
	return ss.hashes
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
	if p, ok := srv.cur().Manifests[assembly]; ok {
		if m, err := manifest.Read(p); err == nil {
			for _, c := range m.Chromosomes {
				for _, seg := range c.Segments {
					h := strings.TrimPrefix(seg.Hash, "blake3:")
					ss.hashes[h] = struct{}{}
					srv.segOf[h] = assembly
				}
			}
			ss.total = len(ss.hashes)
			ss.organism = m.Organism
			ss.version = m.Version
			ss.bases = m.TotalBases
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
	for _, a := range keys(srv.cur().Manifests) {
		srv.segsFor(a)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// httpClient bounds every outbound call (registry pulls, manifest + segment
// fetches) so one slow or dead peer can't stall discover/download.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// getRemoteJSON GETs url and decodes JSON into v. Used by discover to pull the
// upstream registry and remote manifests.
func getRemoteJSON(url string, v any) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
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
