package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// IsControlPath reports whether a request targets a mutating endpoint that must
// be authenticated when a token is configured. Covers the node control plane
// (/actions/*) and the publisher write surface (POST /segments/{hash}, POST
// /genomes/{assembly}/manifest). Reads of those same paths (GET/HEAD segments,
// GET manifest) are not gated — content is public and content-addressed.
func IsControlPath(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/actions/") {
		return true
	}
	if r.Method == http.MethodPost {
		if strings.HasPrefix(r.URL.Path, "/segments/") {
			return true
		}
		if strings.HasPrefix(r.URL.Path, "/genomes/") && strings.HasSuffix(r.URL.Path, "/manifest") {
			return true
		}
	}
	return false
}

// ControlAuth wraps next so control-plane requests require a bearer token.
// When token is "", auth is disabled and next is returned unchanged — the
// caller (serve/node) is responsible for warning the operator that the control
// plane is open. Read endpoints (segments, manifests, catalog, status,
// discover, healthz) are never gated here: content is public and
// content-addressed, so reads stay open by design.
func ControlAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsControlPath(r) {
			got := []byte(r.Header.Get("Authorization"))
			// ConstantTimeCompare is 1 only when equal and same length.
			if subtle.ConstantTimeCompare(got, want) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
