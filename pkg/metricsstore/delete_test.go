package metricsstore_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/metricsstore"
)

// kyber#565: the agent finalizer reaps per-agent time-series on delete.

func TestMemoryMetricsStore_DeleteAgent(t *testing.T) {
	ctx := context.Background()
	store := metricsstore.NewMemoryMetricsStore()

	// dave's activity + token-usage series, plus an unrelated agent + an
	// unrelated namespace that must both survive.
	_ = store.AddPoint(ctx, metricsstore.ActivityKey("ns1", "dave", "working"), 100, 0.5)
	_ = store.AddPoint(ctx, metricsstore.TokenUsageKey("ns1", "dave", "claude-opus-4", "input"), 100, 7)
	_ = store.AddPoint(ctx, metricsstore.ActivityKey("ns1", "han", "working"), 100, 0.5)
	_ = store.AddPoint(ctx, metricsstore.ActivityKey("ns2", "dave", "working"), 100, 0.5)

	if err := store.DeleteAgent(ctx, "ns1", "dave"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	// dave/ns1 series gone.
	for _, key := range []string{
		metricsstore.ActivityKey("ns1", "dave", "working"),
		metricsstore.TokenUsageKey("ns1", "dave", "claude-opus-4", "input"),
	} {
		pts, err := store.RangeQuery(ctx, key, 0, 1<<62)
		if err != nil {
			t.Fatalf("RangeQuery %q: %v", key, err)
		}
		if len(pts) != 0 {
			t.Errorf("series %q must be empty after DeleteAgent, got %d points", key, len(pts))
		}
	}
	// han/ns1 and dave/ns2 must survive (no cross-agent / cross-namespace bleed).
	for _, key := range []string{
		metricsstore.ActivityKey("ns1", "han", "working"),
		metricsstore.ActivityKey("ns2", "dave", "working"),
	} {
		pts, err := store.RangeQuery(ctx, key, 0, 1<<62)
		if err != nil {
			t.Fatalf("RangeQuery %q: %v", key, err)
		}
		if len(pts) != 1 {
			t.Errorf("series %q must survive, got %d points", key, len(pts))
		}
	}
}

func TestMemoryMetricsStore_DeleteAgentMissing_IsIdempotent(t *testing.T) {
	store := metricsstore.NewMemoryMetricsStore()
	if err := store.DeleteAgent(context.Background(), "ns1", "ghost"); err != nil {
		t.Errorf("DeleteAgent on missing: got %v, want nil", err)
	}
}
