package delta

import (
	"bytes"
	"testing"

	"github.com/luizeduardocarvalho/genomehub/internal/aligner"
	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
)

// roundTrip builds a delta from blocks then applies it, asserting the
// reconstructed query equals the original.
func roundTrip(t *testing.T, ref, query []fasta.Chromosome, blocks []aligner.Block) *Delta {
	t.Helper()
	d := Build("Q", "R", ref, query, blocks)
	got, err := Apply(d, ref)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(got) != len(query) {
		t.Fatalf("chrom count: got %d want %d", len(got), len(query))
	}
	for i := range query {
		if got[i].Name != query[i].Name {
			t.Errorf("chrom %d name: got %q want %q", i, got[i].Name, query[i].Name)
		}
		if !bytes.Equal(got[i].Sequence, query[i].Sequence) {
			t.Errorf("chrom %s sequence mismatch:\n got %s\nwant %s", query[i].Name, got[i].Sequence, query[i].Sequence)
		}
	}
	return d
}

func TestRoundTrip_SNP(t *testing.T) {
	// query differs from ref by a single SNP at position 5 (X op).
	ref := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("AAAAACCCCCGGGGG")}}
	query := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("AAAAATCCCCGGGGG")}}
	blocks := []aligner.Block{{
		QueryName: "chr1", QueryStart: 0, QueryEnd: 15,
		TargetName: "chr1", TargetStart: 0, TargetEnd: 15,
		Strand: "+", CIGAR: "5=1X9=",
	}}
	d := roundTrip(t, ref, query, blocks)
	if d.LiteralBases != 1 {
		t.Errorf("LiteralBases: got %d want 1 (one SNP base)", d.LiteralBases)
	}
}

func TestRoundTrip_InsertionAndDeletion(t *testing.T) {
	// query = ref with 2bp insertion after pos 5 and a 3bp deletion later.
	ref := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("AAAAACCCCCGGGGGTTTTT")}}
	// after 5=, insert "NN" (I2), 5= (CCCCC), delete GGG (D3), 7= (GGTTTTT)
	query := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("AAAAANNCCCCCGGTTTTT")}}
	blocks := []aligner.Block{{
		QueryName: "chr1", QueryStart: 0, QueryEnd: 19,
		TargetName: "chr1", TargetStart: 0, TargetEnd: 20,
		Strand: "+", CIGAR: "5=2I5=3D7=",
	}}
	d := roundTrip(t, ref, query, blocks)
	if d.LiteralBases != 2 {
		t.Errorf("LiteralBases: got %d want 2 (insertion only)", d.LiteralBases)
	}
}

func TestRoundTrip_NovelHeadAndTail(t *testing.T) {
	// query has unaligned novel sequence before and after the aligned block.
	ref := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("CCCCCGGGGG")}}
	query := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("TTCCCCCGGGGGAA")}}
	blocks := []aligner.Block{{
		QueryName: "chr1", QueryStart: 2, QueryEnd: 12,
		TargetName: "chr1", TargetStart: 0, TargetEnd: 10,
		Strand: "+", CIGAR: "10=",
	}}
	d := roundTrip(t, ref, query, blocks)
	if d.LiteralBases != 4 { // "TT" + "AA"
		t.Errorf("LiteralBases: got %d want 4", d.LiteralBases)
	}
}

func TestRoundTrip_Unaligned(t *testing.T) {
	// no blocks at all → whole query is literal, still round-trips.
	ref := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("CCCCCGGGGG")}}
	query := []fasta.Chromosome{{Name: "chr2", Sequence: []byte("ACGTACGTAC")}}
	d := roundTrip(t, ref, query, nil)
	if d.LiteralBases != 10 {
		t.Errorf("LiteralBases: got %d want 10 (all novel)", d.LiteralBases)
	}
}

func TestApply_ReferenceMismatch(t *testing.T) {
	ref := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("AAAAA")}}
	query := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("AAAAA")}}
	d := Build("Q", "R", ref, query, []aligner.Block{{
		QueryName: "chr1", QueryStart: 0, QueryEnd: 5,
		TargetName: "chr1", TargetStart: 0, TargetEnd: 5,
		Strand: "+", CIGAR: "5=",
	}})
	wrongRef := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("TTTTT")}}
	if _, err := Apply(d, wrongRef); err == nil {
		t.Fatal("expected reference mismatch error, got nil")
	}
}

func TestBinaryCodecRoundTrip(t *testing.T) {
	ref := []fasta.Chromosome{
		{Name: "chr1", Sequence: []byte("AAAAACCCCCGGGGGTTTTTACGTACGTAC")},
		{Name: "chr2", Sequence: []byte("GGGGGCCCCCAAAAA")},
	}
	query := []fasta.Chromosome{
		// ref with a SNP at pos5 (C→T) and an "NNN" insertion after the first 15 ref bases.
		{Name: "chr1", Sequence: []byte("AAAAATCCCCGGGGGNNNTTTTTACGTACGTAC")},
		{Name: "chr2", Sequence: []byte("GGGGGCCCCCAAAAA")},
	}
	blocks := []aligner.Block{
		{QueryName: "chr1", QueryStart: 0, QueryEnd: 33, TargetName: "chr1", TargetStart: 0, TargetEnd: 30, Strand: "+", CIGAR: "5=1X9=3I15="},
		{QueryName: "chr2", QueryStart: 0, QueryEnd: 15, TargetName: "chr2", TargetStart: 0, TargetEnd: 15, Strand: "+", CIGAR: "15="},
	}
	d := Build("Q", "R", ref, query, blocks)

	dir := t.TempDir()
	path := dir + "/x.ghd"
	if err := d.Write(path); err != nil {
		t.Fatalf("WriteBinary: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("ReadBinary: %v", err)
	}

	// The decoded delta must reconstruct the original query.
	chroms, err := Apply(got, ref)
	if err != nil {
		t.Fatalf("Apply after binary round-trip: %v", err)
	}
	for i := range query {
		if !bytes.Equal(chroms[i].Sequence, query[i].Sequence) {
			t.Errorf("chrom %s mismatch after binary round-trip", query[i].Name)
		}
	}
	if got.Assembly != d.Assembly || got.ReferenceHash != d.ReferenceHash || got.LiteralBases != d.LiteralBases {
		t.Errorf("metadata mismatch: got %+v", got)
	}
}

func TestPack2bit(t *testing.T) {
	seq := []byte("ACGTACGTA")
	packed, ok := pack2bit(seq)
	if !ok {
		t.Fatal("pack2bit returned ok=false for pure ACGT")
	}
	if got := unpack2bit(packed, len(seq)); !bytes.Equal(got, seq) {
		t.Errorf("unpack: got %s want %s", got, seq)
	}
	if _, ok := pack2bit([]byte("ACGTN")); ok {
		t.Error("pack2bit should reject N")
	}
}

func TestCopyMerge(t *testing.T) {
	// adjacent = ops broken by the CIGAR but contiguous in ref should merge to
	// a single copy op.
	ref := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("AAAAACCCCC")}}
	query := []fasta.Chromosome{{Name: "chr1", Sequence: []byte("AAAAACCCCC")}}
	d := Build("Q", "R", ref, query, []aligner.Block{{
		QueryName: "chr1", QueryStart: 0, QueryEnd: 10,
		TargetName: "chr1", TargetStart: 0, TargetEnd: 10,
		Strand: "+", CIGAR: "5=5=",
	}})
	if n := len(d.Chromosomes[0].Ops); n != 1 {
		t.Errorf("expected 1 merged copy op, got %d", n)
	}
}
