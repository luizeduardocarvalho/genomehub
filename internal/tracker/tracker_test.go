package tracker

import (
	"testing"
	"time"
)

func TestAnnounceAndPeers(t *testing.T) {
	r := NewRegistry(time.Minute)
	r.announce(announceReq{NodeID: "n1", Address: "http://a:8080", Kind: "node", Hashes: []string{"h1", "h2"}})
	r.announce(announceReq{NodeID: "n2", Address: "http://b:8080", Kind: "node", Hashes: []string{"h2"}})

	if p := r.peers("h1"); len(p) != 1 || p[0] != "http://a:8080" {
		t.Errorf("peers(h1) = %v, want [http://a:8080]", p)
	}
	if p := r.peers("h2"); len(p) != 2 {
		t.Errorf("peers(h2) = %v, want 2 peers", p)
	}
	if p := r.peers("missing"); len(p) != 0 {
		t.Errorf("peers(missing) = %v, want none", p)
	}
}

func TestReannounceReplacesContent(t *testing.T) {
	r := NewRegistry(time.Minute)
	r.announce(announceReq{NodeID: "n1", Address: "http://a", Hashes: []string{"h1", "h2"}})
	r.announce(announceReq{NodeID: "n1", Address: "http://a", Hashes: []string{"h2", "h3"}})
	if p := r.peers("h1"); len(p) != 0 {
		t.Errorf("h1 should be gone after re-announce, got %v", p)
	}
	if p := r.peers("h3"); len(p) != 1 {
		t.Errorf("h3 should be present after re-announce, got %v", p)
	}
}

func TestLeaveRemovesContent(t *testing.T) {
	r := NewRegistry(time.Minute)
	r.announce(announceReq{NodeID: "n1", Address: "http://a", Hashes: []string{"h1"}})
	r.leave("n1")
	if p := r.peers("h1"); len(p) != 0 {
		t.Errorf("after leave, peers(h1) = %v, want none", p)
	}
	if len(r.nodeViews()) != 0 {
		t.Errorf("after leave, expected no nodes")
	}
}

func TestHeartbeatUnknown(t *testing.T) {
	r := NewRegistry(time.Minute)
	if r.heartbeat("ghost") {
		t.Error("heartbeat for unknown node should return false")
	}
	r.announce(announceReq{NodeID: "n1", Address: "http://a", Hashes: []string{"h1"}})
	if !r.heartbeat("n1") {
		t.Error("heartbeat for known node should return true")
	}
}

func TestExpiry(t *testing.T) {
	r := NewRegistry(20 * time.Millisecond)
	r.announce(announceReq{NodeID: "n1", Address: "http://a", Hashes: []string{"h1"}})
	if len(r.peers("h1")) != 1 {
		t.Fatal("expected peer present before expiry")
	}
	time.Sleep(40 * time.Millisecond)
	// peers() filters stale nodes even before GC runs
	if p := r.peers("h1"); len(p) != 0 {
		t.Errorf("expected no peers after timeout, got %v", p)
	}
	r.GC()
	if len(r.nodeViews()) != 0 {
		t.Errorf("GC should have dropped the stale node")
	}
}
