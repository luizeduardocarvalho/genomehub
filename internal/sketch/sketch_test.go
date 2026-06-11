package sketch

import (
	"math/rand"
	"testing"
)

func randSeq(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	bases := []byte("ACGT")
	s := make([]byte, n)
	for i := range s {
		s[i] = bases[r.Intn(4)]
	}
	return s
}

// mutate returns a copy of seq with ~rate fraction of bases substituted.
func mutate(seq []byte, rate float64, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	bases := []byte("ACGT")
	out := append([]byte(nil), seq...)
	for i := range out {
		if r.Float64() < rate {
			out[i] = bases[r.Intn(4)]
		}
	}
	return out
}

func TestIdentical(t *testing.T) {
	seq := randSeq(50000, 1)
	a := Compute("a", [][]byte{seq}, DefaultKmer, DefaultSize)
	b := Compute("b", [][]byte{seq}, DefaultKmer, DefaultSize)
	if j := Jaccard(a, b); j != 1.0 {
		t.Errorf("identical Jaccard: got %v want 1.0", j)
	}
	if s := Similarity(a, b); s != 1.0 {
		t.Errorf("identical Similarity: got %v want 1.0", s)
	}
}

func TestUnrelated(t *testing.T) {
	a := Compute("a", [][]byte{randSeq(50000, 1)}, DefaultKmer, DefaultSize)
	b := Compute("b", [][]byte{randSeq(50000, 2)}, DefaultKmer, DefaultSize)
	if j := Jaccard(a, b); j > 0.01 {
		t.Errorf("unrelated Jaccard: got %v want ~0", j)
	}
}

func TestSimilarityOrdering(t *testing.T) {
	base := randSeq(100000, 7)
	near := mutate(base, 0.01, 11) // ~99% identity
	far := mutate(base, 0.15, 13)  // ~85% identity
	a := Compute("base", [][]byte{base}, DefaultKmer, DefaultSize)
	n := Compute("near", [][]byte{near}, DefaultKmer, DefaultSize)
	f := Compute("far", [][]byte{far}, DefaultKmer, DefaultSize)

	simNear := Similarity(a, n)
	simFar := Similarity(a, f)
	if !(simNear > simFar) {
		t.Errorf("expected near (%v) > far (%v)", simNear, simFar)
	}
	// 1% divergence should land high (same-species delta range, > 0.95).
	if simNear < 0.95 {
		t.Errorf("near similarity %v < 0.95 (expected same-species range)", simNear)
	}
	// 15% divergence should land in the segment-dedup range, well below 0.95.
	if simFar > 0.95 {
		t.Errorf("far similarity %v > 0.95 (expected diverged range)", simFar)
	}
}

func TestCanonicalStrand(t *testing.T) {
	seq := randSeq(20000, 3)
	rc := reverseComplement(seq)
	a := Compute("fwd", [][]byte{seq}, DefaultKmer, DefaultSize)
	b := Compute("rev", [][]byte{rc}, DefaultKmer, DefaultSize)
	// Reverse complement has the same canonical k-mers → near-identical sketch.
	if j := Jaccard(a, b); j < 0.99 {
		t.Errorf("reverse-complement Jaccard: got %v want ~1.0", j)
	}
}

func reverseComplement(seq []byte) []byte {
	comp := map[byte]byte{'A': 'T', 'T': 'A', 'C': 'G', 'G': 'C'}
	out := make([]byte, len(seq))
	for i, b := range seq {
		out[len(seq)-1-i] = comp[b]
	}
	return out
}

func TestWriteRead(t *testing.T) {
	s := Compute("x", [][]byte{randSeq(10000, 5)}, DefaultKmer, DefaultSize)
	dir := t.TempDir()
	path := dir + "/x.sketch.json"
	if err := s.Write(path); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assembly != s.Assembly || got.Kmer != s.Kmer || !equalU64(got.Hashes, s.Hashes) {
		t.Error("round-trip mismatch")
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
