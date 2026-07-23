package main

import (
	"sync"
	"time"
)

// rateLimiter is a per-key token bucket. It is safe for concurrent use.
//
// Each key (e.g. a client IP) gets an independent bucket that refills at
// `rate` tokens per second up to a maximum of `burst` tokens. A request
// consumes one token; when the bucket is empty the request is rejected.
//
// Stale buckets are reaped periodically by the janitor goroutine started in
// main so a flood of one-off IPs cannot grow the map without bound.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // refill tokens per second
	burst   int     // maximum bucket size

	// sweepInterval controls how often sweep() drops idle buckets. A bucket
	// is considered idle once it has been untouched for longer than sweepAge.
	sweepInterval time.Duration
	sweepAge      time.Duration
	lastSweep     time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate float64, burst int) *rateLimiter {
	return &rateLimiter{
		buckets:       make(map[string]*tokenBucket),
		rate:          rate,
		burst:         burst,
		sweepInterval: 5 * time.Minute,
		sweepAge:      15 * time.Minute,
		lastSweep:     time.Now(),
	}
}

// allow reports whether key may proceed. It lazily refills the bucket and
// consumes one token on success.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: float64(rl.burst), last: now}
		rl.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		b.tokens += elapsed * rl.rate
		if b.tokens > float64(rl.burst) {
			b.tokens = float64(rl.burst)
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have not been touched within the sweepAge window.
// Intended to be called periodically from the janitor.
func (rl *rateLimiter) sweep(now time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.lastSweep = now
	for k, b := range rl.buckets {
		if now.Sub(b.last) > rl.sweepAge {
			delete(rl.buckets, k)
		}
	}
}
