package tokenstore_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/tokenstore"
)

// Compile-time assertion: MemoryAccumulator satisfies Accumulator.
var _ tokenstore.Accumulator = (*tokenstore.MemoryAccumulator)(nil)

// Compile-time assertion: RedisAccumulator satisfies Accumulator.
var _ tokenstore.Accumulator = (*tokenstore.RedisAccumulator)(nil)

func TestAccumulator_IncrBy_basic(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	delta := tokenstore.TokenDelta{Input: 100, CacheCreation: 20, CacheRead: 50, Output: 7}
	if err := a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", delta); err != nil {
		t.Fatalf("IncrBy: %v", err)
	}

	counts, err := a.GetAll(ctx, "ns1")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(counts) != 1 {
		t.Fatalf("want 1 entry, got %d", len(counts))
	}
	c := counts[0]
	if c.Agent != "han" {
		t.Errorf("Agent = %q, want %q", c.Agent, "han")
	}
	if c.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", c.Model, "claude-sonnet-4-6")
	}
	if c.Input != 100 {
		t.Errorf("Input = %d, want 100", c.Input)
	}
	if c.CacheCreation != 20 {
		t.Errorf("CacheCreation = %d, want 20", c.CacheCreation)
	}
	if c.CacheRead != 50 {
		t.Errorf("CacheRead = %d, want 50", c.CacheRead)
	}
	if c.Output != 7 {
		t.Errorf("Output = %d, want 7", c.Output)
	}
}

func TestAccumulator_IncrBy_accumulates(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Input: 100})
	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Input: 200, CacheRead: 30})
	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{CacheCreation: 10})
	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Output: 40})
	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Output: 2})

	counts, _ := a.GetAll(ctx, "ns1")
	if len(counts) != 1 {
		t.Fatalf("want 1 entry, got %d", len(counts))
	}
	c := counts[0]
	if c.Input != 300 {
		t.Errorf("Input = %d, want 300", c.Input)
	}
	if c.CacheCreation != 10 {
		t.Errorf("CacheCreation = %d, want 10", c.CacheCreation)
	}
	if c.CacheRead != 30 {
		t.Errorf("CacheRead = %d, want 30", c.CacheRead)
	}
	if c.Output != 42 {
		t.Errorf("Output = %d, want 42", c.Output)
	}
}

// TestAccumulator_OutputBackCompat pins the pre-upgrade back-compat contract:
// an entry accumulated before the output field existed (deltas carrying no
// Output — the analogue of a Redis hash without the "output" field) must read
// back with Output 0 and no error.
func TestAccumulator_OutputBackCompat(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Input: 100, CacheRead: 30})

	counts, err := a.GetAll(ctx, "ns1")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(counts) != 1 {
		t.Fatalf("want 1 entry, got %d", len(counts))
	}
	if counts[0].Output != 0 {
		t.Errorf("Output = %d, want 0 for a pre-output entry", counts[0].Output)
	}
}

// TestTokenDelta_IsZero_CoversOutput ensures Output participates in IsZero —
// otherwise an output-only delta would be dropped as a no-op.
func TestTokenDelta_IsZero_CoversOutput(t *testing.T) {
	if (tokenstore.TokenDelta{Output: 1}).IsZero() {
		t.Errorf("IsZero() = true for an output-only delta, want false")
	}
	if !(tokenstore.TokenDelta{}).IsZero() {
		t.Errorf("IsZero() = false for the zero delta, want true")
	}
}

// TestAccumulator_OutputOnlyDeltaCreatesEntry proves an output-only delta is
// persisted (the pre-fix code had no output path at all).
func TestAccumulator_OutputOnlyDeltaCreatesEntry(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Output: 5})

	counts, _ := a.GetAll(ctx, "ns1")
	if len(counts) != 1 {
		t.Fatalf("want 1 entry after output-only delta, got %d", len(counts))
	}
	if counts[0].Output != 5 {
		t.Errorf("Output = %d, want 5", counts[0].Output)
	}
}

func TestAccumulator_IncrBy_multipleAgents(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Input: 100})
	_ = a.IncrBy(ctx, "ns1", "luke", "claude-opus-4-7", tokenstore.TokenDelta{Input: 500})

	counts, _ := a.GetAll(ctx, "ns1")
	if len(counts) != 2 {
		t.Fatalf("want 2 entries, got %d", len(counts))
	}
	totals := map[string]int64{}
	for _, c := range counts {
		totals[c.Agent] = c.Input
	}
	if totals["han"] != 100 {
		t.Errorf("han Input = %d, want 100", totals["han"])
	}
	if totals["luke"] != 500 {
		t.Errorf("luke Input = %d, want 500", totals["luke"])
	}
}

func TestAccumulator_GetAll_namespaceIsolation(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Input: 100})
	_ = a.IncrBy(ctx, "ns2", "luke", "claude-sonnet-4-6", tokenstore.TokenDelta{Input: 200})

	counts, _ := a.GetAll(ctx, "ns1")
	if len(counts) != 1 {
		t.Fatalf("ns1: want 1 entry, got %d", len(counts))
	}
	if counts[0].Agent != "han" {
		t.Errorf("ns1 agent = %q, want \"han\"", counts[0].Agent)
	}

	counts2, _ := a.GetAll(ctx, "ns2")
	if len(counts2) != 1 {
		t.Fatalf("ns2: want 1 entry, got %d", len(counts2))
	}
	if counts2[0].Agent != "luke" {
		t.Errorf("ns2 agent = %q, want \"luke\"", counts2[0].Agent)
	}
}

func TestAccumulator_IncrBy_zeroDeltaIsNoop(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	// Zero delta — should not create an entry.
	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{})

	counts, _ := a.GetAll(ctx, "ns1")
	if len(counts) != 0 {
		t.Errorf("want 0 entries after zero delta, got %d", len(counts))
	}
}

func TestAccumulator_GetAll_emptyNamespace(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	counts, err := a.GetAll(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("want 0, got %d", len(counts))
	}
}

func TestAccumulator_IncrBy_multipleModels(t *testing.T) {
	a := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()

	_ = a.IncrBy(ctx, "ns1", "han", "claude-sonnet-4-6", tokenstore.TokenDelta{Input: 100})
	_ = a.IncrBy(ctx, "ns1", "han", "claude-opus-4-7", tokenstore.TokenDelta{Input: 50})

	counts, _ := a.GetAll(ctx, "ns1")
	if len(counts) != 2 {
		t.Fatalf("want 2 model entries for han, got %d", len(counts))
	}
	totals := map[string]int64{}
	for _, c := range counts {
		totals[c.Model] = c.Input
	}
	if totals["claude-sonnet-4-6"] != 100 {
		t.Errorf("sonnet Input = %d, want 100", totals["claude-sonnet-4-6"])
	}
	if totals["claude-opus-4-7"] != 50 {
		t.Errorf("opus Input = %d, want 50", totals["claude-opus-4-7"])
	}
}
