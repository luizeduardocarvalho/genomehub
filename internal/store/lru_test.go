package store

import (
	"fmt"
	"testing"
)

func TestStoreLRUEviction(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Each segment is 1000 bytes; cap at 3500 → at most 3 fit.
	if err := s.SetCacheLimit(3500); err != nil {
		t.Fatal(err)
	}
	seg := func(i int) []byte {
		b := make([]byte, 1000)
		copy(b, fmt.Sprintf("seg-%d-", i))
		return b
	}

	var hashes []string
	for i := 0; i < 5; i++ {
		h, err := s.Put(seg(i))
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}

	// Cap respected.
	cur, max := s.CacheStats()
	if max != 3500 || cur > 3500 {
		t.Fatalf("cur=%d max=%d, want cur<=3500 max=3500", cur, max)
	}

	// Oldest two (0,1) evicted; newest three (2,3,4) retained.
	for _, i := range []int{0, 1} {
		if has, _ := s.Has(hashes[i]); has {
			t.Fatalf("seg %d should have been evicted", i)
		}
	}
	for _, i := range []int{2, 3, 4} {
		if has, _ := s.Has(hashes[i]); !has {
			t.Fatalf("seg %d should be retained", i)
		}
	}
}

func TestStoreLRURecencyOnGet(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	s.SetCacheLimit(3500) // 3 of 1000-byte segments

	mk := func(tag string) []byte { b := make([]byte, 1000); copy(b, tag); return b }
	h0, _ := s.Put(mk("a"))
	h1, _ := s.Put(mk("b"))
	h2, _ := s.Put(mk("c"))

	// Touch h0 so it is most-recently-used; adding a 4th should now evict h1.
	if _, err := s.Get(h0); err != nil {
		t.Fatal(err)
	}
	h3, _ := s.Put(mk("d"))

	if has, _ := s.Has(h1); has {
		t.Fatal("h1 (least recently used after Get h0) should be evicted")
	}
	for _, h := range []string{h0, h2, h3} {
		if has, _ := s.Has(h); !has {
			t.Fatalf("%s should be retained", h)
		}
	}
}
