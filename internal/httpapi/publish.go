package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
)

// maxUpload caps a single uploaded segment or manifest body. Generous enough for
// a large max-chunk config, small enough that a single request can't exhaust
// memory. A genome is many segments, each bounded by this.
const maxUpload = 64 << 20 // 64 MiB

// hasSegment answers a cheap existence probe (HEAD /segments/{hash}) so a
// publisher uploads only the segments the origin is missing — the
// content-addressed dedup win, in reverse.
func (srv *server) hasSegment(w http.ResponseWriter, r *http.Request) {
	h := strings.TrimPrefix(r.PathValue("hash"), "blake3:")
	if ok, _ := srv.store.Has(h); ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// putSegment stores an uploaded segment (POST /segments/{hash}). The uploader is
// untrusted: the body must hash to the claimed hash or it is rejected, so a
// malicious push cannot poison the store with mislabeled content. Idempotent —
// re-uploading an existing segment is a no-op 200.
func (srv *server) putSegment(w http.ResponseWriter, r *http.Request) {
	claimed := strings.TrimPrefix(r.PathValue("hash"), "blake3:")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	got := store.HashBytes(body)
	if got != claimed {
		http.Error(w, "hash mismatch: body hashes to "+got+", not "+claimed, http.StatusBadRequest)
		return
	}
	if has, _ := srv.store.Has(got); has {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := srv.store.Put(body); err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// receiveManifest accepts a published manifest (POST /genomes/{assembly}/manifest)
// but only after verifying every segment it references is already in the store,
// so the catalog never gains a manifest that can't be reconstructed. Segments
// must therefore be uploaded before the manifest.
func (srv *server) receiveManifest(w http.ResponseWriter, r *http.Request) {
	if srv.manifestDir == "" {
		http.Error(w, "node not configured to accept manifests (no manifest dir)", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	m, err := manifest.Parse(body)
	if err != nil {
		http.Error(w, "parse manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if m.Assembly == "" {
		http.Error(w, "manifest has empty assembly", http.StatusBadRequest)
		return
	}
	if pa := r.PathValue("assembly"); pa != "" && pa != m.Assembly {
		http.Error(w, "assembly mismatch: path "+pa+" vs body "+m.Assembly, http.StatusBadRequest)
		return
	}
	var missing int
	for _, c := range m.Chromosomes {
		for _, s := range c.Segments {
			if ok, _ := srv.store.Has(strings.TrimPrefix(s.Hash, "blake3:")); !ok {
				missing++
			}
		}
	}
	if missing > 0 {
		http.Error(w, fmt.Sprintf("%d referenced segments missing; upload them before the manifest", missing), http.StatusUnprocessableEntity)
		return
	}
	if _, err := srv.installManifest(m); err != nil {
		http.Error(w, "install manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "published %s v%d\n", m.Assembly, m.Version)
}
