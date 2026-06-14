package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestControlAuth(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name   string
		token  string
		path   string
		method string
		header string
		want   int
	}{
		{"disabled lets control through", "", "/actions/delete", "POST", "", 200},
		{"reads always open", "secret", "/segments/abc", "GET", "", 200},
		{"control without token rejected", "secret", "/actions/delete", "POST", "", 401},
		{"control wrong token rejected", "secret", "/actions/delete", "POST", "Bearer nope", 401},
		{"control right token allowed", "secret", "/actions/delete", "POST", "Bearer secret", 200},
		{"control prefix-token rejected", "secret", "/actions/delete", "POST", "Bearer secretx", 401},
		{"segment upload gated", "secret", "/segments/abc", "POST", "", 401},
		{"segment upload allowed", "secret", "/segments/abc", "POST", "Bearer secret", 200},
		{"segment HEAD open", "secret", "/segments/abc", "HEAD", "", 200},
		{"manifest push gated", "secret", "/genomes/X/manifest", "POST", "", 401},
		{"manifest push allowed", "secret", "/genomes/X/manifest", "POST", "Bearer secret", 200},
		{"manifest GET open", "secret", "/genomes/X/manifest", "GET", "", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := ControlAuth(c.token, ok)
			req := httptest.NewRequest(c.method, c.path, nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("got %d, want %d", rec.Code, c.want)
			}
		})
	}
}
