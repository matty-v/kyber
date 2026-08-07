package tokenstore_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// kyber#565: the agent finalizer reaps per-agent token state on delete.

func TestMemoryStore_Delete(t *testing.T) {
	s := tokenstore.NewMemoryStore()
	ctx := context.Background()
	if err := s.Put(ctx, "dave", &tokenreport.Snapshot{Tokens: tokenreport.Tokens{Used: 42}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "dave"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if got != nil {
		t.Errorf("Get after Delete returned %+v, want nil", got)
	}
}

func TestMemoryStore_DeleteMissing_IsIdempotent(t *testing.T) {
	s := tokenstore.NewMemoryStore()
	if err := s.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("Delete on missing key: got %v, want nil", err)
	}
}

func TestMemoryAccumulator_DeleteAgent(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()
	// Two models for dave + one for an unrelated agent that must survive.
	_ = a.IncrBy(ctx, "ns1", "dave", "claude-opus-4", tokenstore.TokenDelta{Input: 10})
	_ = a.IncrBy(ctx, "ns1", "dave", "claude-sonnet-4-6", tokenstore.TokenDelta{Input: 20})
	_ = a.IncrBy(ctx, "ns1", "han", "claude-opus-4", tokenstore.TokenDelta{Input: 5})

	if err := a.DeleteAgent(ctx, "ns1", "dave"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	counts, err := a.GetAll(ctx, "ns1")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, c := range counts {
		if c.Agent == "dave" {
			t.Errorf("dave's accumulator rows must be gone, found %+v", c)
		}
	}
	if len(counts) != 1 || counts[0].Agent != "han" {
		t.Errorf("only han must remain, got %+v", counts)
	}
}

func TestMemoryAccumulator_DeleteAgentMissing_IsIdempotent(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	if err := a.DeleteAgent(context.Background(), "ns1", "ghost"); err != nil {
		t.Errorf("DeleteAgent on missing: got %v, want nil", err)
	}
}
