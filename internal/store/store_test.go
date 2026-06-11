package store_test

import (
	"bytes"
	"testing"

	"github.com/luizcarvalho/genome-hub/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutGet(t *testing.T) {
	s := openTestStore(t)
	data := []byte("ACGTACGTACGT")

	hash, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("Put returned empty hash")
	}

	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get returned %q, want %q", got, data)
	}
}

func TestPutDeduplication(t *testing.T) {
	s := openTestStore(t)
	data := []byte("ACGTACGTACGT")

	h1, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("same data produced different hashes: %q vs %q", h1, h2)
	}
}

func TestHas(t *testing.T) {
	s := openTestStore(t)
	data := []byte("TTTTCCCCGGGG")

	hash, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}

	ok, err := s.Has(hash)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("Has returned false after Put")
	}
}

func TestHasMissing(t *testing.T) {
	s := openTestStore(t)
	ok, err := s.Has("nonexistenthash")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Has returned true for missing key")
	}
}

func TestGetMissing(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Get("nonexistenthash"); err == nil {
		t.Error("expected error for missing key")
	}
}

func TestHashBytes(t *testing.T) {
	a := store.HashBytes([]byte("ACGT"))
	b := store.HashBytes([]byte("ACGT"))
	if a != b {
		t.Errorf("HashBytes not deterministic: %q vs %q", a, b)
	}

	c := store.HashBytes([]byte("TTTT"))
	if a == c {
		t.Error("HashBytes collision between different inputs")
	}
}

func TestPutDistinctData(t *testing.T) {
	s := openTestStore(t)
	h1, _ := s.Put([]byte("AAAA"))
	h2, _ := s.Put([]byte("CCCC"))
	if h1 == h2 {
		t.Error("distinct data produced same hash")
	}

	got1, _ := s.Get(h1)
	got2, _ := s.Get(h2)
	if !bytes.Equal(got1, []byte("AAAA")) || !bytes.Equal(got2, []byte("CCCC")) {
		t.Error("retrieved wrong data for distinct keys")
	}
}
