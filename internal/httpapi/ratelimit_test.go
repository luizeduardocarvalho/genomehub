package httpapi

import (
	"testing"
	"time"
)

func TestIPLimiterAllow(t *testing.T) {
	l := newIPLimiter(10, 2) // 10/s, burst 2
	now := time.Now()

	// Burst of 2 allowed immediately, 3rd denied.
	if !l.allow("1.1.1.1", now) || !l.allow("1.1.1.1", now) {
		t.Fatal("burst should allow first two")
	}
	if l.allow("1.1.1.1", now) {
		t.Fatal("third request in the same instant should be denied")
	}

	// A different IP has its own bucket.
	if !l.allow("2.2.2.2", now) {
		t.Fatal("separate IP should not be rate-limited by the first")
	}

	// After 0.1s, 1 token refills (10/s).
	if !l.allow("1.1.1.1", now.Add(100*time.Millisecond)) {
		t.Fatal("a token should have refilled after 100ms")
	}
}

func TestIPLimiterPrune(t *testing.T) {
	l := newIPLimiter(1, 1)
	now := time.Now()
	l.allow("1.1.1.1", now)
	l.prune(now.Add(time.Hour), 10*time.Minute)
	if len(l.buckets) != 0 {
		t.Fatalf("idle bucket should have been pruned, have %d", len(l.buckets))
	}
}
