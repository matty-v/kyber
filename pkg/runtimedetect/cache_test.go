package runtimedetect_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

func TestMemoryCache_GetEmpty_ReturnsErrCacheEmpty(t *testing.T) {
	c := runtimedetect.NewMemoryCache()
	got, err := c.Get(context.Background())
	if !errors.Is(err, runtimedetect.ErrCacheEmpty) {
		t.Fatalf("expected ErrCacheEmpty, got err=%v snap=%v", err, got)
	}
	if got != nil {
		t.Fatalf("expected nil snapshot on empty cache, got %+v", got)
	}
}

func TestMemoryCache_PutThenGet_RoundTrip(t *testing.T) {
	c := runtimedetect.NewMemoryCache()
	ctx := context.Background()
	snap := &runtimedetect.Snapshot{
		ClaudeCodeVersions: []string{"1.0.0", "0.9.0"},
		Models: []runtimedetect.Model{
			{ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7", ContextWindow: 200_000, ContextWindowKnown: false},
		},
		FetchedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := c.Put(ctx, snap); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.ClaudeCodeVersions) != 2 || got.ClaudeCodeVersions[0] != "1.0.0" {
		t.Fatalf("versions round-trip wrong: %+v", got.ClaudeCodeVersions)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "claude-opus-4-7" {
		t.Fatalf("models round-trip wrong: %+v", got.Models)
	}
	if got.Models[0].ContextWindowKnown {
		t.Fatalf("contextWindowKnown should round-trip as false")
	}
}

// Mutating the snapshot returned from Get must not bleed into the cache —
// otherwise a handler that re-uses its snapshot slice could corrupt the
// cached blob seen by the next request.
func TestMemoryCache_GetReturnsDefensiveCopy(t *testing.T) {
	c := runtimedetect.NewMemoryCache()
	ctx := context.Background()
	snap := &runtimedetect.Snapshot{
		ClaudeCodeVersions: []string{"a", "b"},
		Models:             []runtimedetect.Model{{ID: "m1"}},
	}
	if err := c.Put(ctx, snap); err != nil {
		t.Fatalf("Put: %v", err)
	}
	first, _ := c.Get(ctx)
	first.ClaudeCodeVersions[0] = "MUTATED"
	first.Models[0].ID = "MUTATED"
	second, _ := c.Get(ctx)
	if second.ClaudeCodeVersions[0] != "a" {
		t.Fatalf("cache corrupted via returned slice: %q", second.ClaudeCodeVersions[0])
	}
	if second.Models[0].ID != "m1" {
		t.Fatalf("cache models corrupted via returned slice: %q", second.Models[0].ID)
	}
}

// Mutating the caller's snapshot after Put must not bleed into the cache.
func TestMemoryCache_PutTakesDefensiveCopy(t *testing.T) {
	c := runtimedetect.NewMemoryCache()
	ctx := context.Background()
	snap := &runtimedetect.Snapshot{
		ClaudeCodeVersions: []string{"a"},
	}
	if err := c.Put(ctx, snap); err != nil {
		t.Fatalf("Put: %v", err)
	}
	snap.ClaudeCodeVersions[0] = "MUTATED"
	got, _ := c.Get(ctx)
	if got.ClaudeCodeVersions[0] != "a" {
		t.Fatalf("cache corrupted via caller mutation: %q", got.ClaudeCodeVersions[0])
	}
}

func TestMemoryCache_AgentCatalogIsIndependentFromSnapshot(t *testing.T) {
	c := runtimedetect.NewMemoryCache()
	ctx := context.Background()
	models := []runtimedetect.Model{{ID: "claude-opus", ContextWindow: 1_000_000, ContextWindowKnown: true}}
	if err := c.PutAgentModels(ctx, "alice", models); err != nil {
		t.Fatalf("PutAgentModels: %v", err)
	}
	if _, err := c.Get(ctx); !errors.Is(err, runtimedetect.ErrCacheEmpty) {
		t.Fatalf("agent catalog made public snapshot non-empty: %v", err)
	}
	if err := c.Put(ctx, &runtimedetect.Snapshot{ClaudeCodeVersions: []string{"2.1.0"}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.GetAgentModels(ctx, "alice")
	if err != nil || len(got) != 1 || got[0].ID != "claude-opus" {
		t.Fatalf("catalog lost after snapshot write: models=%+v err=%v", got, err)
	}
}
