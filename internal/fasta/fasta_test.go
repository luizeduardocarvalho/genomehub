package fasta_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
)

func writeFASTA(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.fa")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestReadSingleSequence(t *testing.T) {
	path := writeFASTA(t, ">chr1\nACGT\nTTTT\n")
	chroms, err := fasta.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(chroms) != 1 {
		t.Fatalf("expected 1 chromosome, got %d", len(chroms))
	}
	if chroms[0].Name != "chr1" {
		t.Errorf("name: got %q", chroms[0].Name)
	}
	if want := "ACGTTTTT"; string(chroms[0].Sequence) != want {
		t.Errorf("sequence: got %q, want %q", chroms[0].Sequence, want)
	}
}

func TestReadMultipleSequences(t *testing.T) {
	path := writeFASTA(t, ">seq1\nAAAA\n>seq2\nCCCC\n>seq3\nGGGG\n")
	chroms, err := fasta.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(chroms) != 3 {
		t.Fatalf("expected 3 chromosomes, got %d", len(chroms))
	}
	names := []string{"seq1", "seq2", "seq3"}
	seqs := []string{"AAAA", "CCCC", "GGGG"}
	for i, c := range chroms {
		if c.Name != names[i] {
			t.Errorf("[%d] name: got %q, want %q", i, c.Name, names[i])
		}
		if string(c.Sequence) != seqs[i] {
			t.Errorf("[%d] sequence: got %q, want %q", i, c.Sequence, seqs[i])
		}
	}
}

func TestReadNameTruncatedAtWhitespace(t *testing.T) {
	path := writeFASTA(t, ">chr1 description goes here\nACGT\n")
	chroms, err := fasta.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if chroms[0].Name != "chr1" {
		t.Errorf("name: got %q, want %q", chroms[0].Name, "chr1")
	}
}

func TestReadSequenceUppercased(t *testing.T) {
	path := writeFASTA(t, ">seq1\nacgtacgt\n")
	chroms, err := fasta.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ACGTACGT"; string(chroms[0].Sequence) != want {
		t.Errorf("sequence not uppercased: got %q", chroms[0].Sequence)
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := fasta.Read("/nonexistent/path.fa"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	original := []fasta.Chromosome{
		{Name: "chr1", Sequence: []byte("ACGTACGT")},
		{Name: "chr2", Sequence: []byte("TTTTCCCC")},
	}
	path := filepath.Join(t.TempDir(), "out.fa")
	if err := fasta.Write(path, original); err != nil {
		t.Fatal(err)
	}
	got, err := fasta.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(original) {
		t.Fatalf("got %d chromosomes, want %d", len(got), len(original))
	}
	for i, c := range got {
		if c.Name != original[i].Name {
			t.Errorf("[%d] name: got %q, want %q", i, c.Name, original[i].Name)
		}
		if string(c.Sequence) != string(original[i].Sequence) {
			t.Errorf("[%d] sequence mismatch", i)
		}
	}
}

func TestWriteReadLongSequence(t *testing.T) {
	seq := make([]byte, 300)
	for i := range seq {
		seq[i] = "ACGT"[i%4]
	}
	original := []fasta.Chromosome{{Name: "long", Sequence: seq}}
	path := filepath.Join(t.TempDir(), "long.fa")
	if err := fasta.Write(path, original); err != nil {
		t.Fatal(err)
	}
	got, err := fasta.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Sequence) != string(seq) {
		t.Error("long sequence round-trip failed")
	}
}
