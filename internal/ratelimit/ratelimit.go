// Package ratelimit provides a per-key token-bucket rate limiter used to
// throttle high-volume credential paths (e.g. /auth/login) per source IP.
// In-process only; sharded by key. A distributed limiter is a follow-up
// when satellites grows beyond a single Fly machine.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter caps the request rate at `rate` requests per `window`, with a
// burst capacity of `rate` (the bucket starts full). Allow returns true
// when a request fits in the bucket and false when the caller should be
// rejected (typically with HTTP 429).
type Limiter struct {
	rate     float64
	capacity float64
	now      func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
}

// New returns a Limiter that allows `rate` requests per `window`. The
// bucket size matches `rate`, so a fresh key may burst the full window's
// allowance instantly before the refill regulates it. When window is
// zero or rate is non-positive, the limiter degrades to allow-all.
func New(rate int, window time.Duration) *Limiter {
	if rate <= 0 || window <= 0 {
		return &Limiter{rate: 0, capacity: 0, now: time.Now, buckets: nil}
	}
	return &Limiter{
		rate:     float64(rate) / window.Seconds(),
		capacity: float64(rate),
		now:      time.Now,
		buckets:  make(map[string]*bucket),
	}
}

// Allow consumes one token for key. Returns true when a token was
// available. The zero-rate Limiter (constructed with rate<=0) always
// returns true so consumers can opt out via configuration.
func (l *Limiter) Allow(key string) bool {
	if l.capacity == 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, lastRefill: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.capacity {
			b.tokens = l.capacity
		}
		b.lastRefill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
