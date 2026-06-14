package tracker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNode serves HEAD /segments/{hash} 200 for held hashes, 404 otherwise.
func fakeNode(held map[string]bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := strings.TrimPrefix(r.URL.Path, "/segments/")
		if r.Method == http.MethodHead && held[h] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestVerifyHeld(t *testing.T) {
	node := fakeNode(map[string]bool{"aaa": true, "bbb": true})
	defer node.Close()
	r := NewRegistry(0)

	// Node truly holds what it announces → passes.
	if err := r.verifyHeld(announceReq{Address: node.URL, Hashes: []string{"aaa", "bbb"}}); err != nil {
		t.Fatalf("honest announce rejected: %v", err)
	}

	// Node announces a hash it does not serve → fails (sample size 3 >= 2, so the
	// liar is always sampled).
	if err := r.verifyHeld(announceReq{Address: node.URL, Hashes: []string{"aaa", "ccc"}}); err == nil {
		t.Fatal("announce of an unheld segment should fail verification")
	}

	// Unreachable address → fails.
	if err := r.verifyHeld(announceReq{Address: "http://127.0.0.1:1", Hashes: []string{"aaa"}}); err == nil {
		t.Fatal("unreachable node should fail verification")
	}
}
