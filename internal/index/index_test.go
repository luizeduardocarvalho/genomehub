package index_test

import (
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/index"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
)

func openIndex(t *testing.T) *index.Index {
	t.Helper()
	idx, err := index.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

// fakeHash returns a deterministic 64-char hex string from a short seed.
func fakeHash(seed string) string {
	h := make([]byte, 64)
	for i := range h {
		h[i] = "0123456789abcdef"[(int(seed[i%len(seed)])+i)%16]
	}
	return string(h)
}

func TestPutRefCount(t *testing.T) {
	idx := openIndex(t)
	hash := fakeHash("abc")

	if err := idx.Put(hash, 100, "TAIR10"); err != nil {
		t.Fatal(err)
	}
	if n, err := idx.RefCount(hash); err != nil || n != 1 {
		t.Errorf("RefCount after 1 put: got %d, want 1", n)
	}

	if err := idx.Put(hash, 100, "Ler0"); err != nil {
		t.Fatal(err)
	}
	if n, err := idx.RefCount(hash); err != nil || n != 2 {
		t.Errorf("RefCount after 2 puts: got %d, want 2", n)
	}
}

func TestPutIdempotent(t *testing.T) {
	idx := openIndex(t)
	hash := fakeHash("idem")

	for i := 0; i < 3; i++ {
		if err := idx.Put(hash, 50, "TAIR10"); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	if n, err := idx.RefCount(hash); err != nil || n != 1 {
		t.Errorf("idempotent Put: RefCount = %d, want 1", n)
	}
}

func TestReferencedBy(t *testing.T) {
	idx := openIndex(t)
	hash := fakeHash("refs")

	for _, g := range []string{"Cvi0", "Ler0", "TAIR10"} {
		if err := idx.Put(hash, 200, g); err != nil {
			t.Fatal(err)
		}
	}

	refs, err := idx.ReferencedBy(hash)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(refs)
	want := []string{"Cvi0", "Ler0", "TAIR10"}
	if len(refs) != len(want) {
		t.Fatalf("got %v, want %v", refs, want)
	}
	for i, r := range refs {
		if r != want[i] {
			t.Errorf("[%d] got %q, want %q", i, r, want[i])
		}
	}
}

func TestGenomes(t *testing.T) {
	idx := openIndex(t)
	for _, g := range []string{"Ler0", "TAIR10", "Cvi0"} {
		if err := idx.Put(fakeHash(g), 100, g); err != nil {
			t.Fatal(err)
		}
	}
	genomes, err := idx.Genomes()
	if err != nil {
		t.Fatal(err)
	}
	if len(genomes) != 3 {
		t.Fatalf("got %d genomes, want 3: %v", len(genomes), genomes)
	}
	// Genomes() returns sorted slice
	want := []string{"Cvi0", "Ler0", "TAIR10"}
	for i, g := range genomes {
		if g != want[i] {
			t.Errorf("[%d] got %q, want %q", i, g, want[i])
		}
	}
}

func TestDelete(t *testing.T) {
	idx := openIndex(t)
	hash := fakeHash("del")

	idx.Put(hash, 100, "TAIR10")
	idx.Put(hash, 100, "Ler0")

	if err := idx.Delete(hash, "TAIR10"); err != nil {
		t.Fatal(err)
	}
	if n, _ := idx.RefCount(hash); n != 1 {
		t.Errorf("RefCount after Delete: got %d, want 1", n)
	}

	// Delete last reference — seg: entry should also be removed.
	if err := idx.Delete(hash, "Ler0"); err != nil {
		t.Fatal(err)
	}
	if n, _ := idx.RefCount(hash); n != 0 {
		t.Errorf("RefCount after deleting all refs: got %d, want 0", n)
	}
}

func TestSummaryTwoGenomes(t *testing.T) {
	idx := openIndex(t)
	sharedHash := fakeHash("shared")
	onlyA := fakeHash("onlyA0")
	onlyB := fakeHash("onlyB0")

	idx.Put(sharedHash, 1000, "A")
	idx.Put(sharedHash, 1000, "B")
	idx.Put(onlyA, 500, "A")
	idx.Put(onlyB, 300, "B")

	s, err := idx.Summary()
	if err != nil {
		t.Fatal(err)
	}

	if s.TotalSegments != 3 {
		t.Errorf("TotalSegments: got %d, want 3", s.TotalSegments)
	}
	if s.CoreBytes != 1000 {
		t.Errorf("CoreBytes: got %d, want 1000", s.CoreBytes)
	}
	if s.UniqueBytes != 500+300 {
		t.Errorf("UniqueBytes: got %d, want 800", s.UniqueBytes)
	}
	if s.PartialBytes != 0 {
		t.Errorf("PartialBytes: got %d, want 0", s.PartialBytes)
	}
	if s.StoredBytes != 1000+500+300 {
		t.Errorf("StoredBytes: got %d, want 1800", s.StoredBytes)
	}
	if s.NaiveBytes != 1000*2+500+300 {
		t.Errorf("NaiveBytes: got %d, want 2800", s.NaiveBytes)
	}
	if s.UniquePerGenome["A"] != 500 {
		t.Errorf("UniquePerGenome[A]: got %d, want 500", s.UniquePerGenome["A"])
	}
	if s.UniquePerGenome["B"] != 300 {
		t.Errorf("UniquePerGenome[B]: got %d, want 300", s.UniquePerGenome["B"])
	}
}

func TestSummaryThreeGenomes(t *testing.T) {
	idx := openIndex(t)
	core := fakeHash("core00")
	partial := fakeHash("part00") // shared by A and B only
	uniqA := fakeHash("uniqA0")
	uniqB := fakeHash("uniqB0")
	uniqC := fakeHash("uniqC0")

	idx.Put(core, 900, "A")
	idx.Put(core, 900, "B")
	idx.Put(core, 900, "C")
	idx.Put(partial, 400, "A")
	idx.Put(partial, 400, "B")
	idx.Put(uniqA, 100, "A")
	idx.Put(uniqB, 200, "B")
	idx.Put(uniqC, 300, "C")

	s, err := idx.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if s.CoreBytes != 900 {
		t.Errorf("CoreBytes: got %d, want 900", s.CoreBytes)
	}
	if s.PartialBytes != 400 {
		t.Errorf("PartialBytes: got %d, want 400", s.PartialBytes)
	}
	if s.UniqueBytes != 100+200+300 {
		t.Errorf("UniqueBytes: got %d, want 600", s.UniqueBytes)
	}
}

func TestRebuild(t *testing.T) {
	idx := openIndex(t)
	dir := t.TempDir()

	// Write two synthetic manifests.
	writeManifest := func(assembly, hash1, hash2 string, l1, l2 int) string {
		m := &manifest.Manifest{
			Version:   1,
			Assembly:  assembly,
			CreatedAt: time.Now().UTC(),
			Chromosomes: []manifest.Chromosome{
				{
					Name:   "chr1",
					Length: l1 + l2,
					Hash:   "blake3:" + hash1,
					Segments: []manifest.Segment{
						{Hash: "blake3:" + hash1, Length: l1},
						{Hash: "blake3:" + hash2, Length: l2},
					},
				},
			},
		}
		m.TotalBases = l1 + l2
		m.SegmentsRoot = manifest.ComputeSegmentsRoot(m.Chromosomes)
		path := filepath.Join(dir, assembly+".manifest.json")
		if err := m.Write(path); err != nil {
			t.Fatal(err)
		}
		return path
	}

	shared := fakeHash("shared")
	onlyX := fakeHash("onlyX0")
	onlyY := fakeHash("onlyY0")

	pathX := writeManifest("X", shared, onlyX, 500, 300)
	pathY := writeManifest("Y", shared, onlyY, 500, 400)

	if err := idx.Rebuild([]string{pathX, pathY}); err != nil {
		t.Fatal(err)
	}

	if n, _ := idx.RefCount(shared); n != 2 {
		t.Errorf("shared RefCount: got %d, want 2", n)
	}
	if n, _ := idx.RefCount(onlyX); n != 1 {
		t.Errorf("onlyX RefCount: got %d, want 1", n)
	}

	// Rebuild again — result must be identical (idempotent).
	if err := idx.Rebuild([]string{pathX, pathY}); err != nil {
		t.Fatal(err)
	}
	if n, _ := idx.RefCount(shared); n != 2 {
		t.Errorf("after second Rebuild, shared RefCount: got %d, want 2", n)
	}
}
