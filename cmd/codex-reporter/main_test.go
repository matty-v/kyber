package main

import (
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
