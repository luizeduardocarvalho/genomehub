package httpapi

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
)

// ChromCoverage is per-chromosome coverage for the download picker.
type ChromCoverage struct {
	Name     string `json:"name"`
	Segments int    `json:"segments"`
	Have     int    `json:"have"`
}

// manifestFor returns a genome's manifest from the local catalog, or fetched
// from the upstream registry if not held locally.
func (srv *server) manifestFor(assembly string) (*manifest.Manifest, error) {
	if p, ok := srv.cur().Manifests[assembly]; ok {
		return manifest.Read(p)
	}
	if srv.registryURL == "" {
		return nil, fmt.Errorf("%s not local and no registry configured", assembly)
	}
	var m manifest.Manifest
	if err := getRemoteJSON(srv.registryURL+"/genomes/"+assembly+"/manifest", &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// chromCoverage reports, per chromosome, how many of its segments this node holds.
func (srv *server) chromCoverage(assembly string) ([]ChromCoverage, error) {
	m, err := srv.manifestFor(assembly)
	if err != nil {
		return nil, err
	}
	out := make([]ChromCoverage, 0, len(m.Chromosomes))
	for _, c := range m.Chromosomes {
		have := 0
		for _, seg := range c.Segments {
			if has, _ := srv.store.Has(strings.TrimPrefix(seg.Hash, "blake3:")); has {
				have++
			}
		}
		out = append(out, ChromCoverage{Name: c.Name, Segments: len(c.Segments), Have: have})
	}
	return out, nil
}

// chromHashes returns the segment-hash set for the named chromosomes (all of them
// when chroms is empty).
func (srv *server) chromHashes(assembly string, chroms []string) (map[string]struct{}, error) {
	m, err := srv.manifestFor(assembly)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, c := range chroms {
		want[c] = true
	}
	out := map[string]struct{}{}
	for _, c := range m.Chromosomes {
		if len(want) > 0 && !want[c.Name] {
			continue
		}
		for _, seg := range c.Segments {
			out[strings.TrimPrefix(seg.Hash, "blake3:")] = struct{}{}
		}
	}
	return out, nil
}

// DownloadStatus is a live snapshot of an in-node download, surfaced in /status
// so the TUI shows progress without a separate poll.
type DownloadStatus struct {
	Assembly string `json:"assembly"`
	State    string `json:"state"` // running | paused | done | error | cancelled
	Have     int    `json:"have"`
	Total    int    `json:"total"`
	Bytes    int64  `json:"bytes"` // bytes pulled this run
	Error    string `json:"error,omitempty"`
}

// dlTask is a running (or finished) per-genome download. Counters are atomic;
// state/error are mutex-guarded. Pause is a flag the fetch loop honours between
// segments; cancel is a context — both safe because every segment is atomic
// (verified, then Put), so stopping never leaves broken data.
type dlTask struct {
	assembly string
	chroms   []string // empty = whole genome
	total    int
	have     atomic.Int64
	bytes    atomic.Int64
	paused   atomic.Bool
	cancel   context.CancelFunc

	mu     sync.Mutex
	state  string
	errMsg string
}

func (t *dlTask) setState(s string) { t.mu.Lock(); t.state = s; t.mu.Unlock() }
func (t *dlTask) setErr(e string)   { t.mu.Lock(); t.state, t.errMsg = "error", e; t.mu.Unlock() }

func (t *dlTask) active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state == "running" || t.state == "paused"
}

func (t *dlTask) snapshot() DownloadStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return DownloadStatus{
		Assembly: t.assembly, State: t.state, Have: int(t.have.Load()),
		Total: t.total, Bytes: t.bytes.Load(), Error: t.errMsg,
	}
}

// downloads returns a snapshot of every download task (running and finished).
func (srv *server) downloads() []DownloadStatus {
	srv.dlMu.Lock()
	defer srv.dlMu.Unlock()
	out := make([]DownloadStatus, 0, len(srv.dlTasks))
	for _, t := range srv.dlTasks {
		out = append(out, t.snapshot())
	}
	return out
}

// startDownload begins (or no-ops if already active) fetching a genome's missing
// segments into the local store. It runs in the node — the only process that can
// write the store while it serves — and first tracks the manifest so coverage and
// the Seeding tab update live as segments land.
func (srv *server) startDownload(assembly string, chroms []string) error {
	if srv.registryURL == "" {
		return fmt.Errorf("no upstream registry configured")
	}
	srv.dlMu.Lock()
	if srv.dlTasks == nil {
		srv.dlTasks = map[string]*dlTask{}
	}
	if t, ok := srv.dlTasks[assembly]; ok && t.active() {
		srv.dlMu.Unlock()
		return nil // already downloading
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &dlTask{assembly: assembly, chroms: chroms, cancel: cancel, state: "running"}
	srv.dlTasks[assembly] = t
	srv.dlMu.Unlock()

	go srv.runDownload(ctx, t)
	return nil
}

// pauseDownload / resumeDownload / cancelDownload control an in-flight task.
func (srv *server) pauseDownload(assembly string)  { srv.flipDownload(assembly, "pause") }
func (srv *server) resumeDownload(assembly string) { srv.flipDownload(assembly, "resume") }
func (srv *server) cancelDownload(assembly string) { srv.flipDownload(assembly, "cancel") }

func (srv *server) flipDownload(assembly, op string) {
	srv.dlMu.Lock()
	t := srv.dlTasks[assembly]
	srv.dlMu.Unlock()
	if t == nil {
		return
	}
	switch op {
	case "pause":
		if t.active() {
			t.paused.Store(true)
			t.setState("paused")
		}
	case "resume":
		if t.paused.Load() {
			t.paused.Store(false)
			t.setState("running")
		}
	case "cancel":
		t.cancel()
	}
}

// probeDelta returns true if the registry (or the local catalog) serves a delta
// for assembly — the fast check that routes runDownload to the delta path.
func (srv *server) probeDelta(assembly string) bool {
	cur := srv.cur()
	if _, ok := cur.Recipes[assembly]; ok {
		return true
	}
	if _, ok := cur.Deltas[assembly]; ok {
		return true
	}
	if srv.registryURL == "" {
		return false
	}
	resp, err := httpClient.Get(srv.registryURL + "/deltas/" + assembly)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// runDeltaDownload fetches all recipe chunks for a delta-encoded genome into the
// store (peer-routed, same worker pool as runDownload), then kicks off a
// separate download task for the reference genome so it is available for
// reconstruction and re-serving. Pause/cancel work exactly as for manifests.
func (srv *server) runDeltaDownload(ctx context.Context, t *dlTask) {
	cur := srv.cur()
	_, hasRecipe := cur.Recipes[t.assembly]
	_, hasDelta := cur.Deltas[t.assembly]

	var refAssembly string
	if hasRecipe || hasDelta {
		refAssembly = srv.localRefAssembly(t.assembly)
	} else {
		var err error
		refAssembly, err = srv.trackDelta(t.assembly)
		if err != nil {
			t.setErr(err.Error())
			return
		}
	}

	// Raw delta blob (not recipe-backed): the file is served directly from disk;
	// there are no individual chunks to fetch into the store.
	if _, ok := srv.cur().Recipes[t.assembly]; !ok {
		t.total = 1
		t.have.Add(1)
		if refAssembly != "" {
			srv.startDownload(refAssembly, nil) // background; ignore error
		}
		t.setState("done")
		return
	}

	hashes, err := srv.recipeChunkHashes(t.assembly)
	if err != nil {
		t.setErr(err.Error())
		return
	}
	t.total = len(hashes)

	var missing []string
	for h := range hashes {
		if has, _ := srv.store.Has(h); has {
			t.have.Add(1)
		} else {
			missing = append(missing, h)
		}
	}

	const workers = 8
	ch := make(chan string)
	var wg sync.WaitGroup
	var failOnce sync.Once
	var failErr error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range ch {
				for t.paused.Load() {
					select {
					case <-ctx.Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
				}
				if ctx.Err() != nil {
					return
				}
				n, err := srv.fetchSegment(h)
				if err != nil {
					failOnce.Do(func() { failErr = err; t.cancel() })
					return
				}
				t.have.Add(1)
				t.bytes.Add(int64(n))
			}
		}()
	}
	for _, h := range missing {
		select {
		case <-ctx.Done():
			goto drain
		case ch <- h:
		}
	}
drain:
	close(ch)
	wg.Wait()

	switch {
	case failErr != nil:
		t.setErr(failErr.Error())
	case ctx.Err() != nil:
		t.setState("cancelled")
	default:
		if refAssembly != "" {
			srv.startDownload(refAssembly, nil) // kick off reference; its own task
		}
		t.setState("done")
	}
}

// runDownload detects whether assembly is a delta or a manifest genome and
// routes accordingly. Honours pause/cancel between segments in both paths.
func (srv *server) runDownload(ctx context.Context, t *dlTask) {
	if srv.probeDelta(t.assembly) {
		srv.runDeltaDownload(ctx, t)
		return
	}
	// Track the manifest first: puts the genome in the catalog so Seeding coverage
	// climbs live, and gives us the segment hash set.
	if _, err := srv.trackManifest(t.assembly); err != nil {
		t.setErr(err.Error())
		return
	}
	hashes, err := srv.chromHashes(t.assembly, t.chroms)
	if err != nil {
		t.setErr(err.Error())
		return
	}
	t.total = len(hashes)

	// Partition: count what we already hold, queue the rest.
	var missing []string
	for h := range hashes {
		if has, _ := srv.store.Has(h); has {
			t.have.Add(1)
		} else {
			missing = append(missing, h)
		}
	}

	// Fetch the missing segments with a small worker pool (peers first, origin
	// fallback). Workers honour pause between segments and stop on cancel.
	const workers = 8
	ch := make(chan string)
	var wg sync.WaitGroup
	var failOnce sync.Once
	var failErr error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range ch {
				for t.paused.Load() {
					select {
					case <-ctx.Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
				}
				if ctx.Err() != nil {
					return
				}
				n, err := srv.fetchSegment(h)
				if err != nil {
					failOnce.Do(func() { failErr = err; t.cancel() })
					return
				}
				t.have.Add(1)
				t.bytes.Add(int64(n))
			}
		}()
	}
	for _, h := range missing {
		select {
		case <-ctx.Done():
			goto drain
		case ch <- h:
		}
	}
drain:
	close(ch)
	wg.Wait()

	switch {
	case failErr != nil:
		t.setErr(failErr.Error())
	case ctx.Err() != nil:
		t.setState("cancelled")
	default:
		t.setState("done")
	}
}

// fetchSegment pulls one content-addressed segment — peers from the tracker
// first, then the origin registry — verifies its hash, and stores it.
func (srv *server) fetchSegment(hash string) (int, error) {
	for _, peer := range srv.trackerPeers(hash) {
		if n, ok := srv.tryFetch(strings.TrimRight(peer, "/")+"/segments/"+hash, hash); ok {
			return n, nil
		}
	}
	if srv.registryURL != "" {
		if n, ok := srv.tryFetch(srv.registryURL+"/segments/"+hash, hash); ok {
			return n, nil
		}
	}
	return 0, fmt.Errorf("segment %s: no source", hash)
}

// tryFetch GETs a segment from one URL, verifies, and stores it. ok=false on any
// failure so the caller falls through to the next source.
func (srv *server) tryFetch(url, hash string) (int, bool) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || store.HashBytes(body) != hash {
		return 0, false
	}
	if _, err := srv.store.Put(body); err != nil {
		return 0, false
	}
	return len(body), true
}

// trackerPeers asks the tracker which peers hold a segment. Empty when no tracker
// is configured (then download uses the registry/origin only).
func (srv *server) trackerPeers(hash string) []string {
	if srv.trackerURL == "" {
		return nil
	}
	var resp struct {
		Peers []string `json:"peers"`
	}
	if err := getRemoteJSON(srv.trackerURL+"/peers/"+hash, &resp); err != nil {
		return nil
	}
	// The tracker returns peers sorted; shuffle so load spreads across holders
	// instead of always hitting the same (e.g. origin) first.
	rand.Shuffle(len(resp.Peers), func(i, j int) { resp.Peers[i], resp.Peers[j] = resp.Peers[j], resp.Peers[i] })
	return resp.Peers
}
