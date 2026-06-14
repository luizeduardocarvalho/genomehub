package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/luizeduardocarvalho/genomehub/internal/delta"
	"github.com/luizeduardocarvalho/genomehub/internal/events"
	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/sign"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	"github.com/spf13/cobra"
)

var (
	downloadServer    string
	downloadAssembly  string
	downloadOutput    string
	downloadTracker   string
	downloadParallel  int
	downloadVerifyKey string
)

var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download a genome from a node, fetching only segments not already local",
	Long: `Fetches a genome from a serving node and reconstructs its FASTA. Segments
already present in the local store are not re-downloaded. If the genome is stored
as a delta, its reference genome is resolved (downloaded if missing) and the delta
applied. Each segment is re-hashed on arrival, so the source need not be trusted.

Example:
  genomehub download --server http://origin:8080 --assembly Ler0 --output Ler0.fa`,
	RunE: runDownload,
}

func init() {
	downloadCmd.Flags().StringVar(&downloadServer, "server", "", "origin node base URL, e.g. http://origin:8080 (required; used as fallback)")
	downloadCmd.Flags().StringVar(&downloadTracker, "tracker", "", "tracker URL; when set, segments are fetched from peers first, origin as fallback")
	downloadCmd.Flags().StringVar(&downloadAssembly, "assembly", "", "assembly to download (required)")
	downloadCmd.Flags().StringVar(&downloadOutput, "output", "", "output FASTA path (required)")
	downloadCmd.Flags().IntVar(&downloadParallel, "parallel", 8, "number of segments to fetch concurrently (across peers)")
	downloadCmd.Flags().StringVar(&downloadVerifyKey, "verify-key", "", "origin's ed25519 public key (hex or file); require + verify a signed manifest")
	downloadCmd.MarkFlagRequired("server")
	downloadCmd.MarkFlagRequired("assembly")
	downloadCmd.MarkFlagRequired("output")
	rootCmd.AddCommand(downloadCmd)
}

// downloader holds per-run client state and transfer accounting. Counters are
// atomic because segments are fetched concurrently.
type downloader struct {
	server   string
	tracker  string
	store    *store.Store
	parallel int

	verifyPub string // origin pubkey hex; when set, the manifest signature is required + verified

	wire       atomic.Int64 // bytes received over the network
	fetched    atomic.Int64 // segments fetched
	skipped    atomic.Int64 // segments already local
	fromPeer   atomic.Int64 // segments fetched from a peer (not origin)
	fromOrigin atomic.Int64 // segments fetched from origin
}

func runDownload(_ *cobra.Command, _ []string) error {
	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	d := &downloader{
		server:   strings.TrimRight(downloadServer, "/"),
		tracker:  strings.TrimRight(downloadTracker, "/"),
		store:    s,
		parallel: downloadParallel,
	}
	if downloadVerifyKey != "" {
		pub, err := sign.ResolvePublic(downloadVerifyKey)
		if err != nil {
			return fmt.Errorf("verify key: %w", err)
		}
		d.verifyPub = pub
	}

	chroms, err := d.fetchGenome(downloadAssembly)
	if err != nil {
		return err
	}

	if err := fasta.Write(downloadOutput, chroms); err != nil {
		return err
	}

	note := ""
	if d.tracker != "" {
		note = fmt.Sprintf("%d from peers, %d from origin", d.fromPeer.Load(), d.fromOrigin.Load())
	}
	events.Append(eventsPath(), events.Event{
		Op:       events.Download,
		Assembly: downloadAssembly,
		Bytes:    d.wire.Load(),
		Segments: int(d.fetched.Load()),
		Note:     note,
	})

	fmt.Printf("\ndownloaded %s → %s\n", downloadAssembly, downloadOutput)
	fmt.Printf("  segments: %d fetched, %d already local\n", d.fetched.Load(), d.skipped.Load())
	if d.tracker != "" {
		fmt.Printf("  sources: %d from peers, %d from origin\n", d.fromPeer.Load(), d.fromOrigin.Load())
	}
	fmt.Printf("  over the wire: %s\n", fmtBytesInt(d.wire.Load()))
	return nil
}

// isGHD1 reports whether body is a raw delta blob (vs a JSON recipe).
func isGHD1(body []byte) bool {
	return len(body) >= 4 && string(body[:4]) == "GHD1"
}

// handleManifestSig fetches the manifest's detached signature, verifies it
// against the pinned origin key when --verify-key is set, and caches it beside
// the manifest so this node can relay it. A missing signature is fatal only
// when verification was requested.
func (d *downloader) handleManifestSig(assembly string, manifestBody []byte) error {
	sig, status, err := d.get("/genomes/" + assembly + "/manifest.sig")
	if err != nil {
		if d.verifyPub != "" {
			return fmt.Errorf("fetch signature for %s: %w", assembly, err)
		}
		return nil
	}
	if status != http.StatusOK {
		if d.verifyPub != "" {
			return fmt.Errorf("manifest %s is not signed (status %d) but --verify-key was given", assembly, status)
		}
		return nil
	}
	if d.verifyPub != "" {
		ok, err := sign.Verify(d.verifyPub, manifestBody, sig)
		if err != nil {
			return fmt.Errorf("verify %s: %w", assembly, err)
		}
		if !ok {
			return fmt.Errorf("manifest %s signature does not match --verify-key (tampered or wrong origin)", assembly)
		}
		fmt.Fprintf(os.Stderr, "manifest %s signature verified\n", assembly)
	}
	saveManifestSig(assembly, sig)
	return nil
}

// saveManifestSig caches a manifest's signature beside its manifest in the cache
// dir (named to match the server's <manifest>.sig lookup), so a node serving
// this box relays the origin's signature unchanged.
func saveManifestSig(assembly string, sig []byte) {
	dir := manifestCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, assembly+".manifest.json.sig"), sig, 0o644)
}

// saveManifestCache writes a fetched manifest to the manifest cache dir so a
// co-located node can serve it and report seeding coverage. Best-effort: a
// failure here never fails the download.
func saveManifestCache(assembly string, body []byte) {
	dir := manifestCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, assembly+".manifest.json"), body, 0o644)
}

// fetchGenome reconstructs a genome by assembly: a delta (recipe or raw) if the
// node has one, otherwise a segment manifest. Shared by `download` and `work`.
func (d *downloader) fetchGenome(assembly string) ([]fasta.Chromosome, error) {
	body, status, err := d.get("/deltas/" + assembly)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusOK:
		if isGHD1(body) {
			return d.fromDelta(body)
		}
		return d.fromDeltaRecipe(body)
	case status == http.StatusNotFound || status == http.StatusForbidden:
		// No delta for this assembly → it's a manifest genome. Static hosts
		// (object storage / CDN) sometimes answer a missing key with 403 rather
		// than 404, so treat both as "absent" and fall through to the manifest.
		return d.fromManifest(assembly)
	default:
		return nil, fmt.Errorf("GET /deltas/%s: status %d", assembly, status)
	}
}

// fromDeltaRecipe reassembles a chunked delta: fetch its chunks (peer-parallel,
// like genome segments), concatenate in order into the delta blob, then proceed
// exactly as a raw delta. This is what lets deltas swarm across peers.
func (d *downloader) fromDeltaRecipe(recipeJSON []byte) ([]fasta.Chromosome, error) {
	var r delta.Recipe
	if err := json.Unmarshal(recipeJSON, &r); err != nil {
		return nil, fmt.Errorf("parse delta recipe: %w", err)
	}
	fmt.Fprintf(os.Stderr, "%s delta: %d chunks (%d bytes)\n", r.Assembly, len(r.Chunks), r.TotalSize)

	hashes := make([]string, len(r.Chunks))
	for i, c := range r.Chunks {
		hashes[i] = c.Hash
	}
	if err := d.fetchAll(hashes); err != nil {
		return nil, err
	}

	blob := make([]byte, 0, r.TotalSize)
	for _, c := range r.Chunks {
		data, err := d.store.Get(strings.TrimPrefix(c.Hash, "blake3:"))
		if err != nil {
			return nil, fmt.Errorf("delta chunk %s missing after fetch: %w", c.Hash, err)
		}
		blob = append(blob, data...)
	}
	return d.fromDelta(blob)
}

// fromDelta reconstructs a delta-encoded genome, resolving its reference first.
func (d *downloader) fromDelta(deltaBytes []byte) ([]fasta.Chromosome, error) {
	tmp, err := os.CreateTemp("", "ghd-*.delta")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(deltaBytes); err != nil {
		return nil, err
	}
	tmp.Close()

	dl, err := delta.Read(tmp.Name())
	if err != nil {
		return nil, fmt.Errorf("parse delta: %w", err)
	}
	fmt.Fprintf(os.Stderr, "%s is a delta against %s — resolving reference...\n", dl.Assembly, dl.Reference)

	refChroms, err := d.fromManifest(dl.Reference)
	if err != nil {
		return nil, fmt.Errorf("resolve reference %s: %w", dl.Reference, err)
	}
	return delta.Apply(dl, refChroms)
}

// fromManifest downloads a genome's manifest, fetches any missing segments, and
// reconstructs it (verifying each chromosome hash).
func (d *downloader) fromManifest(assembly string) ([]fasta.Chromosome, error) {
	body, status, err := d.get("/genomes/" + assembly + "/manifest")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET manifest for %s: status %d", assembly, status)
	}
	// Fetch the detached signature (if any). Verify it against the pinned origin
	// key when --verify-key is set — this is what makes a manifest relayed by an
	// untrusted peer trustworthy. The signature is cached so this node can relay
	// it in turn.
	if err := d.handleManifestSig(assembly, body); err != nil {
		return nil, err
	}

	var m manifest.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", assembly, err)
	}

	// Fetch every needed segment concurrently (across peers), then assemble in
	// order from the local store.
	var allHashes []string
	for _, c := range m.Chromosomes {
		for _, seg := range c.Segments {
			allHashes = append(allHashes, seg.Hash)
		}
	}
	if err := d.fetchAll(allHashes); err != nil {
		return nil, err
	}

	// Persist the manifest beside the store so a node serving this box can name
	// the genome it now holds segments for (seeding coverage + attribution).
	saveManifestCache(assembly, body)

	var chroms []fasta.Chromosome
	for _, c := range m.Chromosomes {
		seq := make([]byte, 0, c.Length)
		for _, seg := range c.Segments {
			hash := strings.TrimPrefix(seg.Hash, "blake3:")
			data, err := d.store.Get(hash)
			if err != nil {
				return nil, fmt.Errorf("segment %s missing after fetch: %w", hash, err)
			}
			seq = append(seq, data...)
		}
		if got := "blake3:" + store.HashBytes(seq); got != c.Hash {
			return nil, fmt.Errorf("chromosome %s: integrity check failed", c.Name)
		}
		chroms = append(chroms, fasta.Chromosome{Name: c.Name, Sequence: seq})
	}
	return chroms, nil
}

// fetchAll ensures every (deduplicated) hash is present locally, fetching missing
// ones through a bounded worker pool so different segments stream from different
// peers at once. Returns the first error encountered.
func (d *downloader) fetchAll(hashes []string) error {
	seen := map[string]struct{}{}
	var work []string
	for _, h := range hashes {
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		work = append(work, h)
	}

	n := d.parallel
	if n < 1 {
		n = 1
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, h := range work {
		wg.Add(1)
		sem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := d.ensureSegment(h); err != nil {
				once.Do(func() { firstErr = err })
			}
		}(h)
	}
	wg.Wait()
	return firstErr
}

// ensureSegment returns a segment's bytes from the local store, fetching and
// verifying it when absent. With a tracker configured it tries peers first, then
// falls back to origin. Every fetched segment is re-hashed before it is trusted.
func (d *downloader) ensureSegment(hashWithPrefix string) ([]byte, error) {
	hash := strings.TrimPrefix(hashWithPrefix, "blake3:")
	if has, _ := d.store.Has(hash); has {
		d.skipped.Add(1)
		return d.store.Get(hash)
	}

	// Try peers from the tracker first.
	if d.tracker != "" {
		for _, peer := range d.trackerPeers(hash) {
			body, status, err := d.getURL(strings.TrimRight(peer, "/") + "/segments/" + hash)
			if err != nil || status != http.StatusOK {
				continue
			}
			if store.HashBytes(body) != hash {
				continue // bad bytes from a peer — try the next source
			}
			if _, err := d.store.Put(body); err != nil {
				return nil, err
			}
			d.fetched.Add(1)
			d.fromPeer.Add(1)
			return body, nil
		}
	}

	// Fall back to origin.
	body, status, err := d.getURL(d.server + "/segments/" + hash)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET segment %s: status %d", hash, status)
	}
	if got := store.HashBytes(body); got != hash {
		return nil, fmt.Errorf("segment %s failed verification (got hash %s)", hash, got)
	}
	if _, err := d.store.Put(body); err != nil {
		return nil, err
	}
	d.fetched.Add(1)
	d.fromOrigin.Add(1)
	return body, nil
}

// trackerPeers asks the tracker which nodes hold a hash, returned in random order
// so load spreads across holders instead of always hitting the first one.
func (d *downloader) trackerPeers(hash string) []string {
	body, status, err := d.getURL(d.tracker + "/peers/" + hash)
	if err != nil || status != http.StatusOK {
		return nil
	}
	var r struct {
		Peers []string `json:"peers"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	rand.Shuffle(len(r.Peers), func(i, j int) { r.Peers[i], r.Peers[j] = r.Peers[j], r.Peers[i] })
	return r.Peers
}

// get performs a GET against the origin server.
func (d *downloader) get(path string) ([]byte, int, error) {
	return d.getURL(d.server + path)
}

// getURL performs a GET against an absolute URL, accounting the bytes received.
func (d *downloader) getURL(url string) ([]byte, int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	d.wire.Add(int64(len(body)))
	return body, resp.StatusCode, nil
}
