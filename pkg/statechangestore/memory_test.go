package statechangestore_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/statechangestore"
)

func TestMemoryAccumulator_IncrByAndGetAll(t *testing.T) {
	ctx := context.Background()
	acc := statechangestore.NewMemoryAccumulator()

	_ = acc.IncrBy(ctx, "ns", "han", "working", 3)
	_ = acc.IncrBy(ctx, "ns", "han", "idle", 1)
	_ = acc.IncrBy(ctx, "ns", "luke", "working", 2)

	all, err := acc.GetAll(ctx, "ns")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	// Build a map for easier assertion.
	byKey := map[string]int64{}
	for _, e := range all {
		byKey[e.Agent+":"+e.ToState] = e.Count
	}

	if byKey["han:working"] != 3 {
		t.Errorf("han:working = %d, want 3", byKey["han:working"])
	}
	if byKey["han:idle"] != 1 {
		t.Errorf("han:idle = %d, want 1", byKey["han:idle"])
	}
	if byKey["luke:working"] != 2 {
		t.Errorf("luke:working = %d, want 2", byKey["luke:working"])
	}
}

func TestMemoryAccumulator_ZeroDeltaNoOp(t *testing.T) {
	ctx := context.Background()
	acc := statechangestore.NewMemoryAccumulator()
	_ = acc.IncrBy(ctx, "ns", "han", "working", 0)
	all, _ := acc.GetAll(ctx, "ns")
	if len(all) != 0 {
		t.Errorf("expected no entries after zero-delta IncrBy, got %d", len(all))
	}
}

func TestMemoryAccumulator_NamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	acc := statechangestore.NewMemoryAccumulator()
	_ = acc.IncrBy(ctx, "ns-a", "han", "working", 5)
	_ = acc.IncrBy(ctx, "ns-b", "han", "working", 9)

	aAll, _ := acc.GetAll(ctx, "ns-a")
	if len(aAll) != 1 || aAll[0].Count != 5 {
		t.Errorf("ns-a: want count=5, got %+v", aAll)
	}
	bAll, _ := acc.GetAll(ctx, "ns-b")
	if len(bAll) != 1 || bAll[0].Count != 9 {
		t.Errorf("ns-b: want count=9, got %+v", bAll)
	}
}

func TestMemoryAccumulator_GetAllEmpty(t *testing.T) {
	ctx := context.Background()
	acc := statechangestore.NewMemoryAccumulator()
	all, err := acc.GetAll(ctx, "ns")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("want empty slice, got %d entries", len(all))
	}
}

func TestValidState(t *testing.T) {
	// "unknown" is accepted as of kyber#360 Cause F — it aligns the snapshot
	// path with the CRD path's de facto contract (the runtime's
	// pkg/tokenreport/activity.go exports ActivityUnknown and emits it on
	// detector errors, which the CRD path already accepts).
	for _, s := range []string{"working", "idle", "paused", "unknown"} {
		if !statechangestore.ValidState(s) {
			t.Errorf("ValidState(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "WORKING", "failed", "banana"} {
		if statechangestore.ValidState(s) {
			t.Errorf("ValidState(%q) = true, want false", s)
		}
	}
}
