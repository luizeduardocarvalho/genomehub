package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// ipLimiter is a per-client-IP token bucket: rps tokens added per second, up to
// burst. Cheap and dependency-free; good enough to blunt a flood without a full
// rate-limiting library.
type ipLimiter struct {
	mu      sync.Mutex
	rps     float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(rps, burst float64) *ipLimiter {
	return &ipLimiter{rps: rps, burst: burst, buckets: make(map[string]*bucket)}
}

// allow consumes a token for ip at time now, refilling first. Returns false when
// the bucket is empty (client over its rate).
func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// prune drops buckets idle longer than maxIdle so the map doesn't grow without
// bound as distinct clients come and go.
func (l *ipLimiter) prune(now time.Time, maxIdle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, b := range l.buckets {
		if now.Sub(b.last) > maxIdle {
			delete(l.buckets, ip)
		}
	}
}

// RateLimit limits requests per client IP to rps (with a burst of 2×rps). rps
// <= 0 disables limiting and returns next unchanged. Applied outermost so a
// flood is rejected before auth and store work.
func RateLimit(rps float64, next http.Handler) http.Handler {
	if rps <= 0 {
		return next
	}
	burst := rps * 2
	if burst < 1 {
		burst = 1
	}
	l := newIPLimiter(rps, burst)
	go func() {
		t := time.NewTicker(5 * time.Minute)
		for range t.C {
			l.prune(time.Now(), 10*time.Minute)
		}
	}()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
