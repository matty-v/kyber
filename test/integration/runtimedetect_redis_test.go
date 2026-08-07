//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

// TestRedisCache_PutGet round-trips a Snapshot through Redis. Run with
//
//	go test -tags integration ./test/integration/...
//
// against the docker-compose Redis service in this directory.
func TestRedisCache_PutGet(t *testing.T) {
	cleanRedisKey(t, "runtimedetect:available")
	c := runtimedetect.NewRedisCache(sharedRDB, 0)
	ctx := context.Background()

	snap := &runtimedetect.Snapshot{
		ClaudeCodeVersions: []string{"3.0.0", "2.9.0"},
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
	if len(got.ClaudeCodeVersions) != 2 || got.ClaudeCodeVersions[0] != "3.0.0" {
		t.Fatalf("versions round-trip wrong: %+v", got.ClaudeCodeVersions)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "claude-opus-4-7" {
		t.Fatalf("models round-trip wrong: %+v", got.Models)
	}
}

// TestRedisCache_TTLExpiresToErrCacheEmpty exercises the AC "Redis cache
// TTL behavior" — when the key expires, Get must return ErrCacheEmpty so
// /available falls back to the empty contract.
func TestRedisCache_TTLExpiresToErrCacheEmpty(t *testing.T) {
	cleanRedisKey(t, "runtimedetect:available")
	c := runtimedetect.NewRedisCache(sharedRDB, 2*time.Second)
	ctx := context.Background()

	snap := &runtimedetect.Snapshot{
		ClaudeCodeVersions: []string{"1.0.0"},
	}
	if err := c.Put(ctx, snap); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := c.Get(ctx); err != nil {
		t.Fatalf("Get immediately after Put: %v", err)
	}
	// Wait past the 2s TTL. Redis is wall-clock based here.
	time.Sleep(3 * time.Second)
	_, err := c.Get(ctx)
	if err != runtimedetect.ErrCacheEmpty {
		t.Fatalf("expected ErrCacheEmpty after TTL, got %v", err)
	}
}

// TestRedisCache_ReplicaConsistency simulates two control-plane replicas
// reading from the same Redis: both must return identical Snapshots.
// This is the AC "multiple replicas return the same /available response."
func TestRedisCache_ReplicaConsistency(t *testing.T) {
	cleanRedisKey(t, "runtimedetect:available")
	ctx := context.Background()

	writer := runtimedetect.NewRedisCache(sharedRDB, 0)
	replicaA := runtimedetect.NewRedisCache(sharedRDB, 0)
	replicaB := runtimedetect.NewRedisCache(sharedRDB, 0)

	if err := writer.Put(ctx, &runtimedetect.Snapshot{
		ClaudeCodeVersions: []string{"5.0.0", "4.9.0"},
		Models: []runtimedetect.Model{
			{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", ContextWindow: 200_000, ContextWindowKnown: false},
		},
		FetchedAt: time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("writer Put: %v", err)
	}

	a, err := replicaA.Get(ctx)
	if err != nil {
		t.Fatalf("replicaA Get: %v", err)
	}
	b, err := replicaB.Get(ctx)
	if err != nil {
		t.Fatalf("replicaB Get: %v", err)
	}
	if a.ClaudeCodeVersions[0] != b.ClaudeCodeVersions[0] {
		t.Fatalf("replicas diverged: A=%v B=%v", a.ClaudeCodeVersions, b.ClaudeCodeVersions)
	}
	if len(a.Models) != len(b.Models) {
		t.Fatalf("replicas diverged on models: A=%d B=%d", len(a.Models), len(b.Models))
	}
}
