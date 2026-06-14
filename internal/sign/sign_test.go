package sign

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	s, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("manifest bytes")
	sig := s.Sign(data)

	ok, err := Verify(s.PublicHex(), data, sig)
	if err != nil || !ok {
		t.Fatalf("verify valid = %v, %v; want true, nil", ok, err)
	}

	// Tampered payload fails.
	if ok, _ := Verify(s.PublicHex(), []byte("tampered"), sig); ok {
		t.Fatal("verify tampered data = true; want false")
	}

	// Wrong key fails.
	other, _ := Generate()
	if ok, _ := Verify(other.PublicHex(), data, sig); ok {
		t.Fatal("verify wrong key = true; want false")
	}
}

func TestLoadSignerAndResolvePublic(t *testing.T) {
	s, _ := Generate()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id.key")
	pubPath := filepath.Join(dir, "id.pub")
	os.WriteFile(keyPath, []byte(s.PrivateHex()+"\n"), 0o600)
	os.WriteFile(pubPath, []byte(s.PublicHex()+"\n"), 0o644)

	loaded, err := LoadSigner(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PublicHex() != s.PublicHex() {
		t.Fatal("loaded signer public key mismatch")
	}

	// ResolvePublic from file and from inline hex must agree.
	fromFile, err := ResolvePublic(pubPath)
	if err != nil || fromFile != s.PublicHex() {
		t.Fatalf("ResolvePublic(file) = %q, %v", fromFile, err)
	}
	fromHex, err := ResolvePublic(s.PublicHex())
	if err != nil || fromHex != s.PublicHex() {
		t.Fatalf("ResolvePublic(hex) = %q, %v", fromHex, err)
	}
}
