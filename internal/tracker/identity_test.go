package tracker

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/sign"
)

func signedReq(op string, sg *sign.Signer, hashes []string, ts int64) announceReq {
	id := sg.PublicHex()
	addr := "http://node:8080"
	msg := CanonicalMessage(op, id, addr, ts, HashesDigest(hashes))
	return announceReq{
		NodeID: id, Address: addr, Hashes: hashes, Timestamp: ts,
		Signature: hex.EncodeToString(sg.Sign(msg)),
	}
}

func TestRegistryVerify(t *testing.T) {
	sg, _ := sign.Generate()
	r := NewRegistry(0)
	now := time.Now().Unix()

	if err := r.verify("announce", signedReq("announce", sg, []string{"a", "b"}, now)); err != nil {
		t.Fatalf("valid signed announce rejected: %v", err)
	}

	// Hash set tampered after signing.
	bad := signedReq("announce", sg, []string{"a"}, now)
	bad.Hashes = []string{"a", "x"}
	if err := r.verify("announce", bad); err == nil {
		t.Fatal("tampered hash set should fail verification")
	}

	// Announce signature replayed as a leave.
	if err := r.verify("leave", signedReq("announce", sg, nil, now)); err == nil {
		t.Fatal("operation mismatch should fail verification")
	}

	// Stale timestamp.
	stale := signedReq("announce", sg, nil, now-int64((10*time.Minute).Seconds()))
	if err := r.verify("announce", stale); err == nil {
		t.Fatal("stale timestamp should fail verification")
	}

	// Impersonation: another key's signature under this id.
	other, _ := sign.Generate()
	imp := signedReq("announce", other, nil, now)
	imp.NodeID = sg.PublicHex() // claim sg's identity with other's signature
	if err := r.verify("announce", imp); err == nil {
		t.Fatal("signature from a different key should fail")
	}

	// Unsigned: allowed by default, rejected when identity required.
	unsigned := announceReq{NodeID: "http://url:8080", Address: "http://url:8080"}
	if err := r.verify("announce", unsigned); err != nil {
		t.Fatalf("unsigned allowed by default, got %v", err)
	}
	r.RequireIdentity = true
	if err := r.verify("announce", unsigned); err == nil {
		t.Fatal("unsigned should be rejected when RequireIdentity is set")
	}
}
