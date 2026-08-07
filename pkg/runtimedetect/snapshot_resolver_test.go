package runtimedetect_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

// countingCache wraps a fixed snapshot (or error) and counts Get calls so the
// TTL-memo test can assert the resolver doesn't re-hit the cache within a TTL
// window.
type countingCache struct {
	snap  *runtimedetect.Snapshot
	err   error
	block time.Duration // when > 0, Get sleeps this long (respecting ctx) before returning
	gets  atomic.Int32
}

func (c *countingCache) Put(_ context.Context, _ *runtimedetect.Snapshot) error { return nil }

func (c *countingCache) Get(ctx context.Context) (*runtimedetect.Snapshot, error) {
	c.gets.Add(1)
	if c.block > 0 {
		select {
		case <-time.After(c.block):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.snap, nil
}

func snapWith(models ...runtimedetect.Model) *runtimedetect.Snapshot {
	return &runtimedetect.Snapshot{Models: models}
}

func TestSnapshotResolver_LookupWindow(t *testing.T) {
	cases := []struct {
		name      string
		snap      *runtimedetect.Snapshot
		err       error
		modelID   string
		wantWin   int64
		wantKnown bool
	}{
		{
			name:      "present and known",
			snap:      snapWith(runtimedetect.Model{ID: "claude-opus-4-8", ContextWindow: 1_000_000, ContextWindowKnown: true}),
			modelID:   "claude-opus-4-8",
			wantWin:   1_000_000,
			wantKnown: true,
		},
		{
			name:      "present but ContextWindowKnown=false falls through",
			snap:      snapWith(runtimedetect.Model{ID: "claude-opus-4-8", ContextWindow: runtimedetect.DefaultContextWindowFloor, ContextWindowKnown: false}),
			modelID:   "claude-opus-4-8",
			wantWin:   0,
			wantKnown: false,
		},
		{
			name:      "absent from snapshot",
			snap:      snapWith(runtimedetect.Model{ID: "claude-sonnet-4-6", ContextWindow: 1_000_000, ContextWindowKnown: true}),
			modelID:   "claude-opus-4-8",
			wantWin:   0,
			wantKnown: false,
		},
		{
			name:      "non-positive window is treated as unknown (defense in depth)",
			snap:      snapWith(runtimedetect.Model{ID: "claude-weird", ContextWindow: 0, ContextWindowKnown: true}),
			modelID:   "claude-weird",
			wantWin:   0,
			wantKnown: false,
		},
		{
			name:      "ErrCacheEmpty (poller not yet run) falls through",
			err:       runtimedetect.ErrCacheEmpty,
			modelID:   "claude-opus-4-8",
			wantWin:   0,
			wantKnown: false,
		},
		{
			name:      "cache error (Redis down) falls through",
			err:       errors.New("redis: connection refused"),
			modelID:   "claude-opus-4-8",
			wantWin:   0,
			wantKnown: false,
		},
		{
			name:      "empty model ID",
			snap:      snapWith(runtimedetect.Model{ID: "claude-opus-4-8", ContextWindow: 1_000_000, ContextWindowKnown: true}),
			modelID:   "",
			wantWin:   0,
			wantKnown: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &runtimedetect.SnapshotResolver{
				Cache:   &countingCache{snap: tc.snap, err: tc.err},
				TTL:     30 * time.Second,
				Timeout: 2 * time.Second,
			}
			win, known := r.LookupWindow(context.Background(), tc.modelID)
			if win != tc.wantWin || known != tc.wantKnown {
				t.Errorf("LookupWindow(%q) = (%d, %v), want (%d, %v)", tc.modelID, win, known, tc.wantWin, tc.wantKnown)
			}
		})
	}
}

func TestSnapshotResolver_NilResolverSafe(t *testing.T) {
	var r *runtimedetect.SnapshotResolver
	if win, known := r.LookupWindow(context.Background(), "claude-opus-4-8"); win != 0 || known {
		t.Errorf("nil resolver: got (%d, %v), want (0, false)", win, known)
	}
}

func TestSnapshotResolver_TTLMemoAvoidsRepeatGets(t *testing.T) {
	cc := &countingCache{snap: snapWith(runtimedetect.Model{ID: "claude-opus-4-8", ContextWindow: 1_000_000, ContextWindowKnown: true})}
	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	r := &runtimedetect.SnapshotResolver{
		Cache:   cc,
		TTL:     30 * time.Second,
		Timeout: 2 * time.Second,
		Now:     func() time.Time { return now },
	}

	// First two lookups within the TTL window: only one cache Get.
	r.LookupWindow(context.Background(), "claude-opus-4-8")
	r.LookupWindow(context.Background(), "claude-sonnet-4-6")
	if got := cc.gets.Load(); got != 1 {
		t.Fatalf("within TTL: expected 1 cache Get, got %d", got)
	}

	// Advance past the TTL: the next lookup refreshes.
	now = now.Add(31 * time.Second)
	r.LookupWindow(context.Background(), "claude-opus-4-8")
	if got := cc.gets.Load(); got != 2 {
		t.Fatalf("after TTL expiry: expected 2 cache Gets, got %d", got)
	}
}

func TestSnapshotResolver_BoundedReadTimesOut(t *testing.T) {
	// A cache slower than the timeout must degrade to (0,false), not stall.
	cc := &countingCache{
		snap:  snapWith(runtimedetect.Model{ID: "claude-opus-4-8", ContextWindow: 1_000_000, ContextWindowKnown: true}),
		block: 2 * time.Second,
	}
	r := &runtimedetect.SnapshotResolver{
		Cache:   cc,
		TTL:     30 * time.Second,
		Timeout: 50 * time.Millisecond,
	}
	start := time.Now()
	win, known := r.LookupWindow(context.Background(), "claude-opus-4-8")
	elapsed := time.Since(start)
	if win != 0 || known {
		t.Errorf("timed-out read: got (%d, %v), want (0, false)", win, known)
	}
	if elapsed > time.Second {
		t.Errorf("read was not bounded by Timeout: took %s", elapsed)
	}
}
