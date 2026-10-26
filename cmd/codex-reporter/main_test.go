package main

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCatalogModelsUsesAppServerSchemaWithoutInventingContextWindows(t *testing.T) {
	got := catalogModels([]appServerModel{
		{Model: "gpt-5.6-sol", DisplayName: "GPT-5.6-Sol"},
		{ID: "legacy-id", DisplayName: "Legacy"},
		{Model: "hidden", Hidden: true},
	})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].ID != "gpt-5.6-sol" || got[0].ContextWindow != 0 || got[0].ContextWindowKnown {
		t.Errorf("first model = %+v", got[0])
	}
	if got[1].ID != "legacy-id" {
		t.Errorf("second model = %+v", got[1])
	}
}

// MAT-85: the first model/list at boot can race app-server startup. A failure
// must retry within seconds, backing off, not wait the hourly refresh.
func TestNextCatalogDelayRetriesFailuresSoonAndBacksOff(t *testing.T) {
	failure := errors.New("app-server closed before model/list response")
	wait, retry := nextCatalogDelay(failure, catalogRetryMin)
	if wait != catalogRetryMin || retry != 2*catalogRetryMin {
		t.Fatalf("first failure: wait=%s next=%s", wait, retry)
	}
	for i := 0; i < 10; i++ {
		wait, retry = nextCatalogDelay(failure, retry)
	}
	if wait > catalogRefresh || retry != catalogRefresh {
		t.Fatalf("backoff not capped at the refresh interval: wait=%s next=%s", wait, retry)
	}
	wait, retry = nextCatalogDelay(nil, retry)
	if wait != catalogRefresh || retry != catalogRetryMin {
		t.Fatalf("success: wait=%s next=%s, want hourly refresh and a reset retry", wait, retry)
	}
}

func TestCatalogPreservesNativeEffortAndUnknown(t *testing.T) {
	var input struct {
		Data []appServerModel `json:"data"`
	}
	err := json.Unmarshal([]byte(`{"data":[
 {"model":"known","supportedReasoningEfforts":[{"reasoningEffort":"high","description":"native"},{"reasoningEffort":"ultra"}]},
 {"model":"unknown"},
 {"model":"no-levels","supportedReasoningEfforts":[]},
 {"model":"invalid","supportedReasoningEfforts":[{"reasoningEffort":"high"},{"reasoningEffort":"high"}]}
 ]}`), &input)
	if err != nil {
		t.Fatal(err)
	}
	models := catalogModels(input.Data)
	attachEffortVersion(models, "0.164.0")
	if len(models) != 4 {
		t.Fatalf("models=%+v", models)
	}
	e := models[0].ReasoningEffort
	if e == nil || e.HarnessVersion != "0.164.0" || len(e.Levels) != 2 || e.Levels[1] != "ultra" {
		t.Fatalf("native evidence lost: %+v", e)
	}
	if models[1].ReasoningEffort != nil || models[3].ReasoningEffort != nil {
		t.Fatal("invented evidence for unknown/invalid catalog")
	}
	if models[2].ReasoningEffort == nil || models[2].ReasoningEffort.Levels == nil || len(models[2].ReasoningEffort.Levels) != 0 {
		t.Fatal("explicit empty catalog lost")
	}
	attachEffortVersion(models, "unknown\nversion")
	for _, m := range models {
		if m.ReasoningEffort != nil {
			t.Fatal("invalid client version retained evidence")
		}
	}
}
