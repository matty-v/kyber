package inbound

import (
	"sync"

	"golang.org/x/time/rate"
)

// defaultMaxPerMinute is the per-binding rate when the operator did not set
// Limits.MaxPerMinute on the binding spec.
const defaultMaxPerMinute = 10

// limiterEntry pairs the active token bucket with the maxPerMinute it was
// constructed for, so we can detect operator edits to the binding's limit
// and rebuild the bucket on the next Allow call.
type limiterEntry struct {
	limiter      *rate.Limiter
	maxPerMinute int
}

// rlKey identifies one rate-limit bucket per (agent, binding) pair.
type rlKey struct {
	agent   string
	binding string
}

// RateLimiter is a thread-safe collection of token buckets, one per
// (agent, binding) pair. Buckets are created lazily on first Allow.
type RateLimiter struct {
	mu       sync.Mutex
	limiters map[rlKey]*limiterEntry
}

// NewRateLimiter returns an empty RateLimiter ready for use.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		limiters: make(map[rlKey]*limiterEntry),
	}
}

// Allow consumes one token from the (agent, binding) bucket. It returns true
// if a token was available. Buckets are lazily initialised the first time the
// pair is observed. If maxPerMinute differs from the value the bucket was
// created with, the bucket is reset (operator can edit the CRD without
// stalling traffic indefinitely).
//
// maxPerMinute<=0 falls back to defaultMaxPerMinute, mirroring the CRD's
// "unset means default 10" semantics.
func (r *RateLimiter) Allow(agent, binding string, maxPerMinute int) bool {
	if maxPerMinute <= 0 {
		maxPerMinute = defaultMaxPerMinute
	}
	key := rlKey{agent: agent, binding: binding}

	r.mu.Lock()
	entry, ok := r.limiters[key]
	if !ok || entry.maxPerMinute != maxPerMinute {
		// rate.Every spaces tokens evenly across the minute; burst equals the
		// per-minute cap so a quiet binding can absorb a small spike.
		entry = &limiterEntry{
			limiter:      rate.NewLimiter(rate.Limit(float64(maxPerMinute)/60.0), maxPerMinute),
			maxPerMinute: maxPerMinute,
		}
		r.limiters[key] = entry
	}
	lim := entry.limiter
	r.mu.Unlock()

	return lim.Allow()
}
