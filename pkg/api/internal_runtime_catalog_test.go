package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/runtimedetect"
)

func TestInternalRuntimeCatalogStoresCodexModels(t *testing.T) {
	cache := runtimedetect.NewMemoryCache()
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	srv.SetRuntimeDetectCache(cache)
	req := httptest.NewRequest(http.MethodPost, "/internal/agents/codex-spike/runtime-catalog", bytes.NewBufferString(
		`{"runtime":"codex","models":[{"id":"gpt-5.6-sol","displayName":"GPT-5.6-Sol"}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	snap, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("getting catalog: %v", err)
	}
	if len(snap.CodexModels) != 1 || snap.CodexModels[0].ID != "gpt-5.6-sol" {
		t.Fatalf("CodexModels = %+v", snap.CodexModels)
	}
	if snap.CodexModels[0].ContextWindow != runtimedetect.DefaultContextWindowFloor || snap.CodexModels[0].ContextWindowKnown {
		t.Errorf("context metadata = %+v, want safe unknown floor", snap.CodexModels[0])
	}
}

func TestInternalRuntimeCatalogRejectsWrongRuntime(t *testing.T) {
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	srv.SetRuntimeDetectCache(runtimedetect.NewMemoryCache())
	req := httptest.NewRequest(http.MethodPost, "/internal/agents/alice/runtime-catalog", bytes.NewBufferString(
		`{"runtime":"claude-code","models":[]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
