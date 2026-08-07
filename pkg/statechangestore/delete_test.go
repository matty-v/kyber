package statechangestore_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/statechangestore"
)

// kyber#565: the agent finalizer reaps per-agent state-change counts on delete.

func TestMemoryAccumulator_DeleteAgent(t *testing.T) {
	a := statechangestore.NewMemoryAccumulator()
	ctx := context.Background()
	_ = a.IncrBy(ctx, "ns1", "dave", "working", 3)
	_ = a.IncrBy(ctx, "ns1", "dave", "idle", 2)
	_ = a.IncrBy(ctx, "ns1", "han", "working", 1)

	if err := a.DeleteAgent(ctx, "ns1", "dave"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	counts, err := a.GetAll(ctx, "ns1")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, c := range counts {
		if c.Agent == "dave" {
			t.Errorf("dave's state-change rows must be gone, found %+v", c)
		}
	}
	if len(counts) != 1 || counts[0].Agent != "han" {
		t.Errorf("only han must remain, got %+v", counts)
	}
}

func TestMemoryAccumulator_DeleteAgentMissing_IsIdempotent(t *testing.T) {
	a := statechangestore.NewMemoryAccumulator()
	if err := a.DeleteAgent(context.Background(), "ns1", "ghost"); err != nil {
		t.Errorf("DeleteAgent on missing: got %v, want nil", err)
	}
}
