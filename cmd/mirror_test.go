package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/luizeduardocarvalho/genomehub/internal/sign"
)

func TestWriteMirrorKeyLayout(t *testing.T) {
	dir := t.TempDir()
	// A slashed key must materialize as nested directories + file.
	if err := writeMirror(dir, "genomes/TAIR10/manifest", []byte("m")); err != nil {
		t.Fatal(err)
	}
	if err := writeMirror(dir, "segments/abc123", []byte("s")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"genomes/TAIR10/manifest", "segments/abc123"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key))); err != nil {
			t.Fatalf("expected mirror file for key %q: %v", key, err)
		}
	}
}

func TestMirrorSig(t *testing.T) {
	raw := []byte(`{"assembly":"X"}`)

	// With a signer: a fresh, verifiable signature over the served bytes.
	s, _ := sign.Generate()
	sig, ok := mirrorSig(s, "/nonexistent.manifest.json", raw)
	if !ok {
		t.Fatal("signer should produce a signature")
	}
	if good, _ := sign.Verify(s.PublicHex(), raw, sig); !good {
		t.Fatal("mirror signature should verify against the signer's key")
	}

	// Without a signer: copy an existing .sig if present, else none.
	dir := t.TempDir()
	mpath := filepath.Join(dir, "g.manifest.json")
	os.WriteFile(mpath, raw, 0o644)
	if _, ok := mirrorSig(nil, mpath, raw); ok {
		t.Fatal("no signer and no existing .sig should yield nothing")
	}
	os.WriteFile(mpath+".sig", []byte("existing"), 0o644)
	if got, ok := mirrorSig(nil, mpath, raw); !ok || string(got) != "existing" {
		t.Fatalf("should copy existing .sig, got %q ok=%v", got, ok)
	}
}
