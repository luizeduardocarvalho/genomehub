package manifest_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luizcarvalho/genome-hub/internal/manifest"
)

func sampleChroms() []manifest.Chromosome {
	return []manifest.Chromosome{
		{
			Name:   "chr1",
			Length: 100,
			Hash:   "blake3:aaa",
			Segments: []manifest.Segment{
				{Hash: "blake3:111", Length: 60},
				{Hash: "blake3:222", Length: 40},
			},
		},
		{
			Name:   "chr2",
			Length: 80,
			Hash:   "blake3:bbb",
			Segments: []manifest.Segment{
				{Hash: "blake3:333", Length: 80},
			},
		},
	}
}

func TestComputeSegmentsRootDeterministic(t *testing.T) {
	chroms := sampleChroms()
	a := manifest.ComputeSegmentsRoot(chroms)
	b := manifest.ComputeSegmentsRoot(chroms)
	if a != b {
		t.Errorf("non-deterministic: %q vs %q", a, b)
	}
}

func TestComputeSegmentsRootChangesWithContent(t *testing.T) {
	chroms := sampleChroms()
	root1 := manifest.ComputeSegmentsRoot(chroms)

	chroms[0].Segments[0].Hash = "blake3:999"
	root2 := manifest.ComputeSegmentsRoot(chroms)

	if root1 == root2 {
		t.Error("root should differ when segment hash changes")
	}
}

func TestComputeSegmentsRootEmpty(t *testing.T) {
	r := manifest.ComputeSegmentsRoot(nil)
	if r == "" {
		t.Error("empty chromosome list should still return a hash string")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	chroms := sampleChroms()
	m := &manifest.Manifest{
		Version:      1,
		GraphVersion: 2,
		Assembly:     "TestAssembly",
		TotalBases:   180,
		Encoding:     "raw-ascii",
		Chunking: manifest.Chunking{
			Algorithm: "gear+mem",
			MinSize:   262144,
			MaxSize:   1048576,
		},
		CreatedAt:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		SegmentsRoot: manifest.ComputeSegmentsRoot(chroms),
		Chromosomes:  chroms,
	}

	path := filepath.Join(t.TempDir(), "test.manifest.json")
	if err := m.Write(path); err != nil {
		t.Fatal(err)
	}

	got, err := manifest.Read(path)
	if err != nil {
		t.Fatal(err)
	}

	if got.Assembly != m.Assembly {
		t.Errorf("Assembly: got %q, want %q", got.Assembly, m.Assembly)
	}
	if got.TotalBases != m.TotalBases {
		t.Errorf("TotalBases: got %d, want %d", got.TotalBases, m.TotalBases)
	}
	if got.SegmentsRoot != m.SegmentsRoot {
		t.Errorf("SegmentsRoot: got %q, want %q", got.SegmentsRoot, m.SegmentsRoot)
	}
	if len(got.Chromosomes) != len(m.Chromosomes) {
		t.Fatalf("Chromosomes: got %d, want %d", len(got.Chromosomes), len(m.Chromosomes))
	}
	if len(got.Chromosomes[0].Segments) != len(m.Chromosomes[0].Segments) {
		t.Errorf("chr1 segments: got %d, want %d",
			len(got.Chromosomes[0].Segments), len(m.Chromosomes[0].Segments))
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := manifest.Read("/nonexistent/manifest.json"); err == nil {
		t.Error("expected error for missing file")
	}
}
