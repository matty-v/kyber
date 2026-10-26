package runtimedetect

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEffortEvidenceValidation(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, source, version string
		levels                         []string
		valid                          bool
	}{
		{"native", "codex", "codex-model-list", "0.164.0", []string{"low", "ultra"}, true},
		{"none offered", "codex", "codex-model-list", "0.164.0", []string{}, true},
		{"missing levels", "codex", "codex-model-list", "0.164.0", nil, false},
		{"other runtime", "claude-code", "codex-model-list", "0.164.0", []string{"high"}, false},
		{"other source", "codex", "provider-list", "0.164.0", []string{"high"}, false},
		{"unknown version", "codex", "codex-model-list", "", []string{"high"}, false},
		{"injected version", "codex", "codex-model-list", "0.1\nsecret", []string{"high"}, false},
		{"duplicate", "codex", "codex-model-list", "0.164.0", []string{"high", "high"}, false},
		{"shell fragment", "codex", "codex-model-list", "0.164.0", []string{"$(echo x)"}, false},
		{"unbounded", "codex", "codex-model-list", "0.164.0", []string{strings.Repeat("a", 33)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ReasoningEffortEvidence{Source: tc.source, HarnessVersion: tc.version, Levels: tc.levels}
			if got := e.Validate(tc.runtime); (got == nil) != tc.valid {
				t.Fatalf("validation=%v, valid=%v", got, tc.valid)
			}
		})
	}
	var absent *ReasoningEffortEvidence
	if err := absent.Validate("other"); err != nil {
		t.Fatal(err)
	}
}

func TestEffortEvidenceCacheIsolationAndSerialization(t *testing.T) {
	cache := NewMemoryCache()
	ctx := context.Background()
	input := []Model{{ID: "gpt-example", ReasoningEffort: &ReasoningEffortEvidence{Source: "codex-model-list", HarnessVersion: "0.164.0", Levels: []string{"high"}}}}
	if err := cache.PutAgentModels(ctx, "alice", "codex", input); err != nil {
		t.Fatal(err)
	}
	input[0].ReasoningEffort.Levels[0] = "low"
	first, err := cache.GetAgentModels(ctx, "alice", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if first[0].ReasoningEffort.Levels[0] != "high" {
		t.Fatal("cache aliases input")
	}
	first[0].ReasoningEffort.Levels[0] = "medium"
	second, _ := cache.GetAgentModels(ctx, "alice", "codex")
	if second[0].ReasoningEffort.Levels[0] != "high" {
		t.Fatal("cache aliases output")
	}
	if _, err := cache.GetAgentModels(ctx, "bob", "codex"); err == nil {
		t.Fatal("cross-Agent catalog disclosure")
	}
	for _, levels := range [][]string{nil, {}, {"high"}} {
		m := Model{ID: "m"}
		if levels != nil {
			m.ReasoningEffort = &ReasoningEffortEvidence{Source: "codex-model-list", HarnessVersion: "0.164.0", Levels: levels}
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var got Model
		if err = json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if (got.ReasoningEffort == nil) != (levels == nil) {
			t.Fatalf("unknown/known lost: %s", data)
		}
		if levels != nil && got.ReasoningEffort.Levels == nil {
			t.Fatalf("known empty became unknown: %s", data)
		}
	}
}
