package httpapi_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
)

// TestPublishWriteSurface exercises the origin's write endpoints end-to-end:
// segment existence probe, untrusted upload (with hash-mismatch rejection), and
// manifest acceptance gated on all segments being present.
func TestPublishWriteSurface(t *testing.T) {
	originStore := openStore(t)
	empty, err := httpapi.ScanCatalog(t.TempDir())
	if err != nil {
		t.Fatalf("scan catalog: %v", err)
	}
	manifestDir := t.TempDir()
	origin := httptest.NewServer(httpapi.NewHandler(originStore, empty, "", "", manifestDir, ""))
	defer origin.Close()

	seg := []byte("ACGTACGTACGTACGTACGT")
	hash := store.HashBytes(seg)

	// 1. Not present yet.
	if code := head(t, origin.URL+"/segments/"+hash); code != http.StatusNotFound {
		t.Fatalf("HEAD before upload = %d, want 404", code)
	}

	// 2. Hash-mismatch upload is rejected (untrusted publisher can't mislabel).
	if code, _ := post(t, origin.URL+"/segments/deadbeef", "application/octet-stream", seg); code != http.StatusBadRequest {
		t.Fatalf("mismatched upload = %d, want 400", code)
	}

	// 3. Correct upload, then present and idempotent.
	if code, _ := post(t, origin.URL+"/segments/"+hash, "application/octet-stream", seg); code != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", code)
	}
	if code := head(t, origin.URL+"/segments/"+hash); code != http.StatusOK {
		t.Fatalf("HEAD after upload = %d, want 200", code)
	}
	if code, _ := post(t, origin.URL+"/segments/"+hash, "application/octet-stream", seg); code != http.StatusOK {
		t.Fatalf("re-upload = %d, want 200", code)
	}

	// 4. Manifest referencing an absent segment is rejected.
	missing := fmt.Sprintf(`{"assembly":"X","version":1,"chromosomes":[{"name":"c1","segments":[{"hash":"blake3:%064d","length":1}]}]}`, 0)
	if code, _ := post(t, origin.URL+"/genomes/X/manifest", "application/json", []byte(missing)); code != http.StatusUnprocessableEntity {
		t.Fatalf("dangling manifest = %d, want 422", code)
	}

	// 5. Manifest whose segments are all present is accepted and served.
	good := fmt.Sprintf(`{"assembly":"PUBG","version":1,"chromosomes":[{"name":"c1","segments":[{"hash":"blake3:%s","length":%d}]}]}`, hash, len(seg))
	if code, _ := post(t, origin.URL+"/genomes/PUBG/manifest", "application/json", []byte(good)); code != http.StatusCreated {
		t.Fatalf("publish manifest = %d, want 201", code)
	}
	if code := head(t, origin.URL+"/genomes/PUBG/manifest"); code != http.StatusOK {
		// GET also matches HEAD; just confirm it now resolves.
		t.Fatalf("manifest not served after publish: %d", code)
	}
}

func head(t *testing.T, url string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodHead, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func post(t *testing.T, url, ctype string, body []byte) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, ctype, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
