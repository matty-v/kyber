package runtimedetect

import (
	"context"
	"sync"
	"time"
)

// DefaultSnapshotResolverTTL bounds how long a previously-read detection
// snapshot is reused before the next LookupWindow triggers a fresh cache Get.
// Mirrors contextwindowmap.ResolveCacheTTL (30s): short enough that a model
// detected on the next poll is picked up "soon", long enough that the
// pod-construction hot path makes at most one cache read per window across all
// reconciles rather than one per pod build.
const DefaultSnapshotResolverTTL = 30 * time.Second

// DefaultSnapshotResolverTimeout bounds a single cache Get so a slow or
// unreachable Redis can never stall a reconcile. On timeout the resolver
// degrades to "unknown" and the caller falls through to its floor — the same
// safe direction as an empty snapshot.
const DefaultSnapshotResolverTimeout = 2 * time.Second

// SnapshotResolver exposes the detection poller's per-model context windows
// (the same Snapshot served at GET /api/v1/available) to the synchronous
// pod-construction path, without putting an unbounded Redis GET on every
// reconcile. It wraps a runtimedetect.Cache with a short in-process TTL memo
// and a bounded read — modelled on contextwindowmap.Resolver so nil-safety,
// concurrency, and testability stay consistent across the two window sources.
//
// It is read-only and best-effort: any miss (cache empty, error, timeout,
// model absent, or ContextWindowKnown=false) returns (0, false) so the caller
// falls through to its own fallback. It never blocks pod construction.
type SnapshotResolver struct {
	// Cache is the detection-snapshot cache (shared with /api/v1/available).
	// Required; a nil resolver or nil Cache resolves everything to (0,false).
	Cache Cache
	// TTL bounds in-process memoization. Falls back to
	// DefaultSnapshotResolverTTL when <= 0.
	TTL time.Duration
	// Timeout bounds a single Cache.Get. Falls back to
	// DefaultSnapshotResolverTimeout when <= 0.
	Timeout time.Duration
	// Now is overridable in tests; nil → time.Now.
	Now func() time.Time

	mu       sync.Mutex
	cached   map[string]int64 // modelID → window; only confidently-known, positive entries
	cachedAt time.Time
	hasCache bool
}

// LookupWindow returns (contextWindow, true) when the detection snapshot
// carries a confident, positive window for modelID, else (0, false). nil-safe.
func (r *SnapshotResolver) LookupWindow(ctx context.Context, modelID string) (int64, bool) {
	if r == nil || r.Cache == nil || modelID == "" {
		return 0, false
	}
	m := r.load(ctx)
	if w, ok := m[modelID]; ok {
		return w, true
	}
	return 0, false
}

// load returns the memoized model→window map, refreshing from the cache when
// the memo is stale or empty. The result is memoized for TTL regardless of
// outcome (hit, miss, or error) so a cache outage degrades to one bounded read
// per window rather than one per reconcile. Only models with
// ContextWindowKnown=true AND a positive window are recorded — everything else
// is deliberately absent so LookupWindow falls through.
func (r *SnapshotResolver) load(ctx context.Context) map[string]int64 {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = DefaultSnapshotResolverTTL
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultSnapshotResolverTimeout
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.hasCache && now().Sub(r.cachedAt) < ttl {
		return r.cached
	}

	out := map[string]int64{}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A cache miss, error, or timeout leaves out empty — the caller falls
	// through to its floor, never blocks. Memoize the empty result too so a
	// down cache doesn't get hit every reconcile.
	if snap, err := r.Cache.Get(readCtx); err == nil && snap != nil {
		for _, model := range snap.Models {
			if model.ID != "" && model.ContextWindowKnown && model.ContextWindow > 0 {
				out[model.ID] = model.ContextWindow
			}
		}
	}
	r.cached = out
	r.cachedAt = now()
	r.hasCache = true
	return out
}
