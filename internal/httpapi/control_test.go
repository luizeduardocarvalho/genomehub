package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/delta"
	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
)

// makeGenome writes a manifest (one segment per chromosome) into catalogDir and
// stores each segment in s. Returns the manifest path.
func makeGenome(t *testing.T, s *store.Store, catalogDir, assembly, organism string, chroms map[string][]byte) {
	t.Helper()
	var mchroms []manifest.Chromosome
	bases := 0
	for name, seq := range chroms {
		h, err := s.Put(seq)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		mchroms = append(mchroms, manifest.Chromosome{
			Name: name, Length: len(seq), Hash: "blake3:" + store.HashBytes(seq),
			Segments: []manifest.Segment{{Hash: "blake3:" + h, Length: len(seq)}},
		})
		bases += len(seq)
	}
	m := &manifest.Manifest{Version: 1, Assembly: assembly, Organism: organism, TotalBases: bases, Chromosomes: mchroms}
	if err := m.Write(filepath.Join(catalogDir, assembly+".manifest.json")); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func postJSON(t *testing.T, url string, body any) int {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func seedingHave(t *testing.T, base, assembly string) (have, total int, found bool) {
	t.Helper()
	var st httpapi.Status
	getJSON(t, base+"/status", &st)
	for _, s := range st.Seeding {
		if s.Assembly == assembly {
			return s.Have, s.Total, true
		}
	}
	return 0, 0, false
}

// waitDownload polls until the task for assembly reaches a terminal state.
func waitDownload(t *testing.T, base, assembly string) httpapi.DownloadStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var st httpapi.Status
		getJSON(t, base+"/status", &st)
		for _, d := range st.Downloads {
			if d.Assembly == assembly && d.State != "running" && d.State != "paused" {
				return d
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("download of %s did not finish", assembly)
	return httpapi.DownloadStatus{}
}

// TestControlPlane exercises the full participant loop against an in-process
// origin + peer: registry, discover, track, per-chromosome + full download.
func TestControlPlane(t *testing.T) {
	originStore := openStore(t)
	originCat := t.TempDir()
	makeGenome(t, originStore, originCat, "GEN", "Homo sapiens", map[string][]byte{
		"chr1": bytes.Repeat([]byte("AC"), 100),
		"chr2": bytes.Repeat([]byte("GT"), 100),
		"chr3": bytes.Repeat([]byte("TA"), 100),
	})
	cat, err := httpapi.ScanCatalog(originCat)
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(httpapi.NewHandler(originStore, cat, "", "", "", ""))
	defer origin.Close()

	// Peer: empty store + empty catalog, registry pointed at origin.
	peerStore := openStore(t)
	empty, _ := httpapi.ScanCatalog(t.TempDir())
	peer := httptest.NewServer(httpapi.NewHandler(peerStore, empty, "", origin.URL, t.TempDir(), ""))
	defer peer.Close()

	// registry lists the genome with 3 segments.
	var reg []httpapi.RegistryEntry
	getJSON(t, origin.URL+"/registry", &reg)
	if len(reg) != 1 || reg[0].Assembly != "GEN" || reg[0].Segments != 3 {
		t.Fatalf("registry = %+v", reg)
	}

	// discover: peer holds none yet, but sees it via the registry.
	var disc []httpapi.DiscoverEntry
	getJSON(t, peer.URL+"/discover", &disc)
	if len(disc) != 1 || disc[0].Have != 0 || disc[0].Total != 3 || disc[0].Local {
		t.Fatalf("discover = %+v", disc)
	}

	// track: the genome appears in the peer's seeding at 0/3.
	if code := postJSON(t, peer.URL+"/actions/manifest", map[string]any{"assembly": "GEN"}); code != http.StatusOK {
		t.Fatalf("track status %d", code)
	}
	if have, total, ok := seedingHave(t, peer.URL, "GEN"); !ok || have != 0 || total != 3 {
		t.Fatalf("after track: %d/%d ok=%v", have, total, ok)
	}

	// per-chromosome download: only chr1.
	postJSON(t, peer.URL+"/actions/download", map[string]any{"assembly": "GEN", "chroms": []string{"chr1"}})
	if d := waitDownload(t, peer.URL, "GEN"); d.State != "done" {
		t.Fatalf("chr1 download state %q", d.State)
	}
	if have, _, _ := seedingHave(t, peer.URL, "GEN"); have != 1 {
		t.Fatalf("after chr1 download: have=%d want 1", have)
	}

	// download the rest.
	postJSON(t, peer.URL+"/actions/download", map[string]any{"assembly": "GEN"})
	waitDownload(t, peer.URL, "GEN")
	if have, total, _ := seedingHave(t, peer.URL, "GEN"); have != 3 || total != 3 {
		t.Fatalf("after full download: %d/%d", have, total)
	}
}

// TestReconstruct downloads a genome then rebuilds its FASTA on disk, verifying
// integrity; and checks reconstruct refuses when a segment is missing.
func TestReconstruct(t *testing.T) {
	originStore := openStore(t)
	originCat := t.TempDir()
	makeGenome(t, originStore, originCat, "GEN", "x", map[string][]byte{
		"chr1": bytes.Repeat([]byte("ACGT"), 50),
		"chr2": bytes.Repeat([]byte("TTGG"), 40),
	})
	cat, _ := httpapi.ScanCatalog(originCat)
	origin := httptest.NewServer(httpapi.NewHandler(originStore, cat, "", "", "", ""))
	defer origin.Close()

	peerStore := openStore(t)
	empty, _ := httpapi.ScanCatalog(t.TempDir())
	peer := httptest.NewServer(httpapi.NewHandler(peerStore, empty, "", origin.URL, t.TempDir(), ""))
	defer peer.Close()

	// Reconstruct before download → fails (missing segments).
	if code := postJSON(t, peer.URL+"/actions/reconstruct", map[string]any{"assembly": "GEN", "output": filepath.Join(t.TempDir(), "x.fa")}); code == http.StatusOK {
		t.Fatal("reconstruct should fail before download")
	}

	postJSON(t, peer.URL+"/actions/download", map[string]any{"assembly": "GEN"})
	waitDownload(t, peer.URL, "GEN")

	out := filepath.Join(t.TempDir(), "GEN.fa")
	data, _ := json.Marshal(map[string]any{"assembly": "GEN", "output": out})
	resp, err := http.Post(peer.URL+"/actions/reconstruct", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var res httpapi.ReconstructResult
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if !res.Verified || res.Chromosomes != 2 || res.Bases != 360 {
		t.Fatalf("reconstruct result = %+v want verified 2 chroms 360 bp", res)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("fasta not written: %v", err)
	}
}

// TestDeleteRefcount checks that deleting a genome frees only segments no other
// held genome needs.
func TestDeleteRefcount(t *testing.T) {
	originStore := openStore(t)
	originCat := t.TempDir()
	shared := bytes.Repeat([]byte("AC"), 100)
	makeGenome(t, originStore, originCat, "A", "x", map[string][]byte{"chr1": shared, "chr2": bytes.Repeat([]byte("GG"), 100)})
	makeGenome(t, originStore, originCat, "B", "x", map[string][]byte{"chr1": shared, "chr2": bytes.Repeat([]byte("TT"), 100)})
	cat, _ := httpapi.ScanCatalog(originCat)
	origin := httptest.NewServer(httpapi.NewHandler(originStore, cat, "", "", "", ""))
	defer origin.Close()

	peerStore := openStore(t)
	empty, _ := httpapi.ScanCatalog(t.TempDir())
	peer := httptest.NewServer(httpapi.NewHandler(peerStore, empty, "", origin.URL, t.TempDir(), ""))
	defer peer.Close()

	for _, a := range []string{"A", "B"} {
		postJSON(t, peer.URL+"/actions/download", map[string]any{"assembly": a})
		waitDownload(t, peer.URL, a)
	}

	// A and B share chr1 → 3 distinct segments held.
	var st httpapi.Status
	getJSON(t, peer.URL+"/status", &st)
	if st.SegmentsHeld != 3 {
		t.Fatalf("held = %d want 3", st.SegmentsHeld)
	}

	// Delete A: chr2-of-A is unique (deleted), chr1 is shared with B (kept).
	resp, err := http.Post(peer.URL+"/actions/delete", "application/json", bytes.NewReader([]byte(`{"assembly":"A"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var res httpapi.DeleteResult
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res.Deleted != 1 || res.Kept != 1 {
		t.Fatalf("delete result = %+v want deleted 1 kept 1", res)
	}

	// B intact (still 2/2), store down to 2 segments.
	if have, total, _ := seedingHave(t, peer.URL, "B"); have != 2 || total != 2 {
		t.Fatalf("B after delete A: %d/%d", have, total)
	}
	getJSON(t, peer.URL+"/status", &st)
	if st.SegmentsHeld != 2 {
		t.Fatalf("held after delete = %d want 2", st.SegmentsHeld)
	}
}

// TestUnseedKeepsSegments checks stop-seeding removes the manifest but not data.
func TestUnseedKeepsSegments(t *testing.T) {
	originStore := openStore(t)
	originCat := t.TempDir()
	makeGenome(t, originStore, originCat, "G", "x", map[string][]byte{"chr1": bytes.Repeat([]byte("AC"), 100)})
	cat, _ := httpapi.ScanCatalog(originCat)
	origin := httptest.NewServer(httpapi.NewHandler(originStore, cat, "", "", "", ""))
	defer origin.Close()

	peerStore := openStore(t)
	empty, _ := httpapi.ScanCatalog(t.TempDir())
	peer := httptest.NewServer(httpapi.NewHandler(peerStore, empty, "", origin.URL, t.TempDir(), ""))
	defer peer.Close()

	postJSON(t, peer.URL+"/actions/download", map[string]any{"assembly": "G"})
	waitDownload(t, peer.URL, "G")

	if code := postJSON(t, peer.URL+"/actions/unseed", map[string]any{"assembly": "G"}); code != http.StatusNoContent {
		t.Fatalf("unseed status %d", code)
	}
	var st httpapi.Status
	getJSON(t, peer.URL+"/status", &st)
	if len(st.Seeding) != 0 {
		t.Fatalf("still seeding after unseed: %+v", st.Seeding)
	}
	if st.SegmentsHeld != 1 {
		t.Fatalf("segments dropped on unseed: held=%d want 1", st.SegmentsHeld)
	}
}

// TestDeltaDownload verifies the in-node delta download path: a peer downloads
// a recipe-backed delta genome, all recipe chunks land in its store, seeding
// reports real chunk coverage, and the reference genome is auto-downloaded.
func TestDeltaDownload(t *testing.T) {
	// Reference genome (TAIR10) as an ordered slice so ReferenceHash is stable.
	refChroms := []fasta.Chromosome{
		{Name: "chr1", Sequence: bytes.Repeat([]byte("ACGT"), 25)}, // 100 B
		{Name: "chr2", Sequence: bytes.Repeat([]byte("TGCA"), 25)}, // 100 B
	}

	originStore := openStore(t)
	originCat := t.TempDir()

	// TAIR10: two-chromosome manifest genome on the origin.
	makeGenome(t, originStore, originCat, "TAIR10", "Arabidopsis thaliana", map[string][]byte{
		"chr1": refChroms[0].Sequence,
		"chr2": refChroms[1].Sequence,
	})

	// Ler0: a minimal delta against TAIR10.
	// chr1: copy first 80 B from TAIR10 chr1, then 20 novel bytes.
	// chr2: identical to TAIR10 chr2 (pure copy op).
	ler0Chr1 := append(append([]byte(nil), refChroms[0].Sequence[:80]...), bytes.Repeat([]byte("T"), 20)...)
	ler0Chr2 := refChroms[1].Sequence
	d := &delta.Delta{
		Version:       1,
		Assembly:      "Ler0",
		Reference:     "TAIR10",
		ReferenceHash: delta.ReferenceHash(refChroms),
		TotalBases:    len(ler0Chr1) + len(ler0Chr2),
		LiteralBases:  20,
		Chromosomes: []delta.ChromDelta{
			{
				Name: "chr1", Length: len(ler0Chr1),
				Hash: "blake3:" + store.HashBytes(ler0Chr1),
				Ops: []delta.Op{
					{Type: delta.OpCopy, RefChrom: "chr1", RefStart: 0, RefEnd: 80},
					{Type: delta.OpLiteral, Bytes: bytes.Repeat([]byte("T"), 20)},
				},
			},
			{
				Name: "chr2", Length: len(ler0Chr2),
				Hash: "blake3:" + store.HashBytes(ler0Chr2),
				Ops: []delta.Op{
					{Type: delta.OpCopy, RefChrom: "chr2", RefStart: 0, RefEnd: len(ler0Chr2)},
				},
			},
		},
	}

	// Write the delta blob to disk so we can chunk it.
	deltaPath := filepath.Join(t.TempDir(), "Ler0.delta.ghd")
	if err := d.Write(deltaPath); err != nil {
		t.Fatalf("write delta: %v", err)
	}
	blob, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatal(err)
	}

	// Chunk the blob into small segments (32 B) and store them in originStore.
	const chunkSize = 32
	var chunks []delta.Chunk
	for i := 0; i < len(blob); i += chunkSize {
		end := i + chunkSize
		if end > len(blob) {
			end = len(blob)
		}
		h, err := originStore.Put(blob[i:end])
		if err != nil {
			t.Fatalf("put chunk: %v", err)
		}
		chunks = append(chunks, delta.Chunk{Hash: h, Length: end - i})
	}
	recipe := &delta.Recipe{Assembly: "Ler0", Reference: "TAIR10", TotalSize: len(blob), Chunks: chunks}
	if err := recipe.Write(filepath.Join(originCat, "Ler0.deltarecipe.json")); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	cat, err := httpapi.ScanCatalog(originCat)
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(httpapi.NewHandler(originStore, cat, "", "", "", ""))
	defer origin.Close()

	// Peer: empty store + empty catalog, registry pointed at origin.
	peerStore := openStore(t)
	empty, _ := httpapi.ScanCatalog(t.TempDir())
	peer := httptest.NewServer(httpapi.NewHandler(peerStore, empty, "", origin.URL, t.TempDir(), ""))
	defer peer.Close()

	// discover: peer should see both TAIR10 (manifest) and Ler0 (delta).
	var disc []httpapi.DiscoverEntry
	getJSON(t, peer.URL+"/discover", &disc)
	if len(disc) != 2 {
		t.Fatalf("discover = %d entries, want 2: %+v", len(disc), disc)
	}

	// Download Ler0 via the node control plane.
	postJSON(t, peer.URL+"/actions/download", map[string]any{"assembly": "Ler0"})
	d1 := waitDownload(t, peer.URL, "Ler0")
	if d1.State != "done" {
		t.Fatalf("Ler0 download state %q error=%q", d1.State, d1.Error)
	}
	if d1.Total != len(chunks) {
		t.Fatalf("Ler0 total chunks %d, want %d", d1.Total, len(chunks))
	}

	// Seeding should list Ler0 as a delta with full chunk coverage.
	have, total, ok := seedingHave(t, peer.URL, "Ler0")
	if !ok || total != len(chunks) || have != len(chunks) {
		t.Fatalf("Ler0 seeding: %d/%d ok=%v", have, total, ok)
	}

	// Ler0 download auto-started TAIR10 reference download.
	d2 := waitDownload(t, peer.URL, "TAIR10")
	if d2.State != "done" {
		t.Fatalf("TAIR10 reference download state %q", d2.State)
	}
	haveRef, totalRef, okRef := seedingHave(t, peer.URL, "TAIR10")
	if !okRef || totalRef == 0 || haveRef != totalRef {
		t.Fatalf("TAIR10 seeding after auto-download: %d/%d ok=%v", haveRef, totalRef, okRef)
	}
}

// buildLer0Origin creates an origin with TAIR10 (manifest) and Ler0 (recipe
// delta against TAIR10). Returns the origin server and the number of recipe chunks.
func buildLer0Origin(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	refChroms := []fasta.Chromosome{
		{Name: "chr1", Sequence: bytes.Repeat([]byte("ACGT"), 25)},
		{Name: "chr2", Sequence: bytes.Repeat([]byte("TGCA"), 25)},
	}
	originStore := openStore(t)
	originCat := t.TempDir()
	makeGenome(t, originStore, originCat, "TAIR10", "Arabidopsis thaliana", map[string][]byte{
		"chr1": refChroms[0].Sequence,
		"chr2": refChroms[1].Sequence,
	})

	ler0Chr1 := append(append([]byte(nil), refChroms[0].Sequence[:80]...), bytes.Repeat([]byte("T"), 20)...)
	ler0Chr2 := refChroms[1].Sequence
	d := &delta.Delta{
		Version: 1, Assembly: "Ler0", Reference: "TAIR10",
		ReferenceHash: delta.ReferenceHash(refChroms),
		TotalBases:    len(ler0Chr1) + len(ler0Chr2), LiteralBases: 20,
		Chromosomes: []delta.ChromDelta{
			{
				Name: "chr1", Length: len(ler0Chr1),
				Hash: "blake3:" + store.HashBytes(ler0Chr1),
				Ops: []delta.Op{
					{Type: delta.OpCopy, RefChrom: "chr1", RefStart: 0, RefEnd: 80},
					{Type: delta.OpLiteral, Bytes: bytes.Repeat([]byte("T"), 20)},
				},
			},
			{
				Name: "chr2", Length: len(ler0Chr2),
				Hash: "blake3:" + store.HashBytes(ler0Chr2),
				Ops: []delta.Op{
					{Type: delta.OpCopy, RefChrom: "chr2", RefStart: 0, RefEnd: len(ler0Chr2)},
				},
			},
		},
	}
	deltaPath := filepath.Join(t.TempDir(), "Ler0.delta.ghd")
	if err := d.Write(deltaPath); err != nil {
		t.Fatalf("write delta: %v", err)
	}
	blob, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatal(err)
	}
	const chunkSize = 32
	var chunks []delta.Chunk
	for i := 0; i < len(blob); i += chunkSize {
		end := i + chunkSize
		if end > len(blob) {
			end = len(blob)
		}
		h, err := originStore.Put(blob[i:end])
		if err != nil {
			t.Fatalf("put chunk: %v", err)
		}
		chunks = append(chunks, delta.Chunk{Hash: h, Length: end - i})
	}
	recipe := &delta.Recipe{Assembly: "Ler0", Reference: "TAIR10", TotalSize: len(blob), Chunks: chunks}
	if err := recipe.Write(filepath.Join(originCat, "Ler0.deltarecipe.json")); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	cat, err := httpapi.ScanCatalog(originCat)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(originStore, cat, "", "", "", ""))
	t.Cleanup(srv.Close)
	return srv, len(chunks)
}

// TestDeltaUnseed verifies that unseeding a recipe-backed delta removes it from
// the catalog but leaves all its chunks in the store.
func TestDeltaUnseed(t *testing.T) {
	origin, _ := buildLer0Origin(t)

	peerStore := openStore(t)
	empty, _ := httpapi.ScanCatalog(t.TempDir())
	peer := httptest.NewServer(httpapi.NewHandler(peerStore, empty, "", origin.URL, t.TempDir(), ""))
	defer peer.Close()

	postJSON(t, peer.URL+"/actions/download", map[string]any{"assembly": "Ler0"})
	if d := waitDownload(t, peer.URL, "Ler0"); d.State != "done" {
		t.Fatalf("Ler0 download: %q %q", d.State, d.Error)
	}
	waitDownload(t, peer.URL, "TAIR10")

	var st httpapi.Status
	getJSON(t, peer.URL+"/status", &st)
	heldBefore := st.SegmentsHeld

	if code := postJSON(t, peer.URL+"/actions/unseed", map[string]any{"assembly": "Ler0"}); code != http.StatusNoContent {
		t.Fatalf("unseed status %d", code)
	}

	getJSON(t, peer.URL+"/status", &st)
	for _, s := range st.Seeding {
		if s.Assembly == "Ler0" {
			t.Fatalf("Ler0 still in seeding after unseed")
		}
	}
	if st.SegmentsHeld != heldBefore {
		t.Fatalf("unseed freed chunks: held %d→%d, want unchanged", heldBefore, st.SegmentsHeld)
	}
}

// TestDeltaDelete verifies that deleting a recipe-backed delta frees chunks
// unique to it (none shared with the TAIR10 manifest segments).
func TestDeltaDelete(t *testing.T) {
	origin, numChunks := buildLer0Origin(t)

	peerStore := openStore(t)
	empty, _ := httpapi.ScanCatalog(t.TempDir())
	peer := httptest.NewServer(httpapi.NewHandler(peerStore, empty, "", origin.URL, t.TempDir(), ""))
	defer peer.Close()

	postJSON(t, peer.URL+"/actions/download", map[string]any{"assembly": "Ler0"})
	if d := waitDownload(t, peer.URL, "Ler0"); d.State != "done" {
		t.Fatalf("Ler0 download: %q %q", d.State, d.Error)
	}
	waitDownload(t, peer.URL, "TAIR10")

	var st httpapi.Status
	getJSON(t, peer.URL+"/status", &st)
	heldBefore := st.SegmentsHeld

	resp, err := http.Post(peer.URL+"/actions/delete", "application/json", bytes.NewReader([]byte(`{"assembly":"Ler0"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var res httpapi.DeleteResult
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()

	if res.Deleted != numChunks || res.Kept != 0 {
		t.Fatalf("delete result = %+v, want deleted %d kept 0", res, numChunks)
	}

	getJSON(t, peer.URL+"/status", &st)
	if st.SegmentsHeld != heldBefore-numChunks {
		t.Fatalf("held %d→%d, want %d", heldBefore, st.SegmentsHeld, heldBefore-numChunks)
	}
	for _, s := range st.Seeding {
		if s.Assembly == "Ler0" {
			t.Fatalf("Ler0 still seeding after delete")
		}
	}
	if _, _, ok := seedingHave(t, peer.URL, "TAIR10"); !ok {
		t.Fatalf("TAIR10 gone after Ler0 delete")
	}
}
