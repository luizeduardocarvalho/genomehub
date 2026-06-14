// Package sign provides ed25519 manifest signing for GenomeHub. The origin
// holds a private key and signs the manifests it serves; clients pin the
// origin's public key and verify the detached signature. Because the signature
// travels with the manifest, a peer can relay an origin-signed manifest without
// being able to forge it — closing the untrusted-peer authenticity gap that TLS
// (one transport hop) does not.
package sign

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Signer holds an ed25519 keypair and signs manifest bytes.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// Generate creates a fresh keypair.
func Generate() (*Signer, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	return &Signer{priv: priv, pub: pub}, nil
}

// Sign returns the detached signature over data.
func (s *Signer) Sign(data []byte) []byte { return ed25519.Sign(s.priv, data) }

// PublicHex / PrivateHex render the keys as hex for storage.
func (s *Signer) PublicHex() string  { return hex.EncodeToString(s.pub) }
func (s *Signer) PrivateHex() string { return hex.EncodeToString(s.priv) }

// LoadSigner reads a private key (hex) from a file.
func LoadSigner(path string) (*Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	priv, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	pk := ed25519.PrivateKey(priv)
	return &Signer{priv: pk, pub: pk.Public().(ed25519.PublicKey)}, nil
}

// Verify reports whether sig is a valid signature over data for the given
// public key (hex).
func Verify(pubHex string, data, sig []byte) (bool, error) {
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil {
		return false, fmt.Errorf("decode public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	return ed25519.Verify(ed25519.PublicKey(pub), data, sig), nil
}

// ResolvePublic accepts either a hex public key or a path to a file containing
// one, and returns the hex string. Lets clients pass --verify-key inline or by
// file.
func ResolvePublic(hexOrPath string) (string, error) {
	v := strings.TrimSpace(hexOrPath)
	if v == "" {
		return "", fmt.Errorf("empty key")
	}
	if info, err := os.Stat(v); err == nil && !info.IsDir() {
		b, err := os.ReadFile(v)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return v, nil
}
