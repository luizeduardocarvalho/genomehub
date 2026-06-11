package chunker_test

import (
	"bytes"
	"testing"

	"github.com/luizcarvalho/genome-hub/internal/chunker"
)

var smallCfg = chunker.Config{MinSize: 16, MaxSize: 64}

func TestSplitEmpty(t *testing.T) {
	if chunks := chunker.Split(nil, smallCfg); chunks != nil {
		t.Errorf("nil input should return nil, got %v", chunks)
	}
	if chunks := chunker.Split([]byte{}, smallCfg); chunks != nil {
		t.Errorf("empty input should return nil, got %v", chunks)
	}
}

func TestSplitReassembly(t *testing.T) {
	data := makeSeq(512)
	chunks := chunker.Split(data, smallCfg)
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	got := bytes.Join(chunks, nil)
	if !bytes.Equal(got, data) {
		t.Error("chunks do not reassemble to original data")
	}
}

func TestSplitChunkBounds(t *testing.T) {
	data := makeSeq(4096)
	chunks := chunker.Split(data, smallCfg)
	for i, c := range chunks {
		if i < len(chunks)-1 && len(c) < smallCfg.MinSize {
			t.Errorf("chunk %d length %d below MinSize %d", i, len(c), smallCfg.MinSize)
		}
		if len(c) > smallCfg.MaxSize {
			t.Errorf("chunk %d length %d exceeds MaxSize %d", i, len(c), smallCfg.MaxSize)
		}
	}
}

func TestSplitDeterministic(t *testing.T) {
	data := makeSeq(2048)
	a := chunker.Split(data, smallCfg)
	b := chunker.Split(data, smallCfg)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic: got %d then %d chunks", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Errorf("chunk %d differs between runs", i)
		}
	}
}

func TestSplitLocalityInsert(t *testing.T) {
	data := makeSeq(2048)
	before := chunker.Split(data, smallCfg)

	// insert 4 bytes at the midpoint
	mid := len(data) / 2
	modified := make([]byte, len(data)+4)
	copy(modified, data[:mid])
	copy(modified[mid:], []byte{0xFF, 0xFF, 0xFF, 0xFF})
	copy(modified[mid+4:], data[mid:])
	after := chunker.Split(modified, smallCfg)

	// chunks entirely before the insertion must be identical (guaranteed by boundary reset)
	pos := 0
	for i, c := range before {
		if pos+len(c) > mid {
			break // this chunk straddles or is past the insertion — stop
		}
		if i >= len(after) || !bytes.Equal(c, after[i]) {
			t.Errorf("chunk %d (before insertion) changed but should be stable", i)
		}
		pos += len(c)
	}
}

func TestSplitShorterThanMin(t *testing.T) {
	data := makeSeq(10) // shorter than MinSize=16
	chunks := chunker.Split(data, smallCfg)
	if len(chunks) != 1 {
		t.Errorf("data shorter than MinSize should produce 1 chunk, got %d", len(chunks))
	}
	if !bytes.Equal(chunks[0], data) {
		t.Error("single chunk does not match input")
	}
}

func TestSplitDefaultConfig(t *testing.T) {
	cfg := chunker.Default()
	if cfg.MinSize != chunker.DefaultMinSize || cfg.MaxSize != chunker.DefaultMaxSize {
		t.Errorf("Default() returned unexpected config: %+v", cfg)
	}
}

// makeSeq generates a pseudo-genomic byte sequence of length n.
func makeSeq(n int) []byte {
	bases := []byte("ACGT")
	b := make([]byte, n)
	for i := range b {
		b[i] = bases[(i*7+i/4)%4]
	}
	return b
}
