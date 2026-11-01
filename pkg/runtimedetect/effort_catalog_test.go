package runtimedetect_test

import (
	"context"
	"errors"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	"testing"
	"time"
)

func TestEffortEvidenceIsSeparateAndDefensivelyCopied(t *testing.T) {
	ctx := context.Background()
	cache := runtimedetect.NewMemoryCache()
	if _, err := cache.GetEffortCatalog(ctx, "a"); !errors.Is(err, runtimedetect.ErrCacheEmpty) {
		t.Fatal(err)
	}
	c := runtimedetect.EffortCatalog{AgentUID: "uid", PodUID: "pod", Version: "0.166.0", ObservedAt: time.Now(), Models: []runtimedetect.Model{{ID: "model", ReasoningEffort: &runtimedetect.ReasoningEffortEvidence{Source: "codex-model-list", HarnessVersion: "0.166.0", Levels: []string{"ultra"}}}}}
	if err := cache.PutEffortCatalog(ctx, "a", c); err != nil {
		t.Fatal(err)
	}
	c.Models[0].ReasoningEffort.Levels[0] = "mutated"
	got, err := cache.GetEffortCatalog(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Models[0].ReasoningEffort.Levels[0] != "ultra" {
		t.Fatal("stored input alias")
	}
	got.Models[0].ReasoningEffort.Levels[0] = "mutated"
	got, _ = cache.GetEffortCatalog(ctx, "a")
	if got.Models[0].ReasoningEffort.Levels[0] != "ultra" {
		t.Fatal("returned output alias")
	}
	if _, err := cache.GetEffortCatalog(ctx, "b"); !errors.Is(err, runtimedetect.ErrCacheEmpty) {
		t.Fatal("cross-agent evidence")
	}
	if _, err := cache.GetAgentModels(ctx, "a", "codex"); !errors.Is(err, runtimedetect.ErrCacheEmpty) {
		t.Fatal("effort evidence leaked into picker cache")
	}
}
