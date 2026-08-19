package main

import "testing"

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
