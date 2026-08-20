package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenBucketLimiter is a minimal in-memory, per-key token-bucket rate limiter.
//
// It is deliberately dependency-free (the module has no golang.org/x/time/rate)
// and per-instance: for a single-instance deployment that is sufficient. If the
// service is ever scaled horizontally, swap this for a shared store (Redis /
// etc.) so limits are enforced cluster-wide.
type tokenBucketLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	rate      float64 // tokens added per second
	capacity  float64 // max tokens held (burst size)
	lastSweep time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// newTokenBucketLimiter returns a limiter that refills at ratePerSec tokens per
// second and allows bursts up to capacity requests.
func newTokenBucketLimiter(ratePerSec, capacity float64) *tokenBucketLimiter {
	return &tokenBucketLimiter{
		buckets:   make(map[string]*tokenBucket),
		rate:      ratePerSec,
		capacity:  capacity,
		lastSweep: time.Now(),
	}
}

// Allow reports whether one request is currently allowed for the given key.
func (l *tokenBucketLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: l.capacity, last: now}
		l.buckets[key] = b
	}

	// Refill based on time elapsed since the last request.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.capacity {
			b.tokens = l.capacity
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// sweepLocked periodically drops stale buckets so the map cannot grow without
// bound. Callers must hold l.mu.
func (l *tokenBucketLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < 5*time.Minute {
		return
	}
	for key, b := range l.buckets {
		if now.Sub(b.last) > 30*time.Minute {
			delete(l.buckets, key)
		}
	}
	l.lastSweep = now
}

// clientIP returns the best-effort client IP for rate limiting, honoring the
// X-Forwarded-For / X-Real-IP headers that Apache and Cloudflare set. This is
// NOT a security boundary (those headers are spoofable if the service is
// directly reachable) — it is only used to spread abuse controls per client.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
