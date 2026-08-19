package api_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/runtimedetect"
)

func TestInternalRuntimeCatalogRejectsMissingContextWindow(t *testing.T) {
	cache := runtimedetect.NewMemoryCache()
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	srv.SetRuntimeDetectCache(cache)
	req := httptest.NewRequest(http.MethodPost, "/internal/agents/codex-spike/runtime-catalog", bytes.NewBufferString(
		`{"runtime":"codex","models":[{"id":"gpt-5.6-sol","displayName":"GPT-5.6-Sol"}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestInternalRuntimeCatalogRejectsWrongRuntime(t *testing.T) {
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	srv.SetRuntimeDetectCache(runtimedetect.NewMemoryCache())
	req := httptest.NewRequest(http.MethodPost, "/internal/agents/alice/runtime-catalog", bytes.NewBufferString(
		`{"runtime":"other","models":[]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestInternalRuntimeCatalogStoresClaudeModelsPerAgent(t *testing.T) {
	cache := runtimedetect.NewMemoryCache()
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	srv.SetRuntimeDetectCache(cache)
	for _, tc := range []struct {
		agent  string
		model  string
		window int64
	}{
		{agent: "alice", model: "claude-opus-4-1", window: 1_000_000},
		{agent: "bob", model: "claude-sonnet-4-5", window: 200_000},
	} {
		req := httptest.NewRequest(http.MethodPost, "/internal/agents/"+tc.agent+"/runtime-catalog", bytes.NewBufferString(
			`{"runtime":"claude-code","models":[{"id":"`+tc.model+`","displayName":"Claude","contextWindow":`+fmt.Sprint(tc.window)+`,"contextWindowKnown":true}]}`))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204; body=%s", tc.agent, rr.Code, rr.Body.String())
		}
	}
	snap, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("getting catalog: %v", err)
	}
	if got := snap.AgentModels["alice"]; len(got) != 1 || got[0].ID != "claude-opus-4-1" {
		t.Errorf("alice models = %+v", got)
	} else if got[0].ContextWindow != 1_000_000 || !got[0].ContextWindowKnown {
		t.Errorf("alice context metadata = %+v", got[0])
	}
	if got := snap.AgentModels["bob"]; len(got) != 1 || got[0].ID != "claude-sonnet-4-5" {
		t.Errorf("bob models = %+v", got)
	}
}

func TestInternalRuntimeCatalogRejectsInvalidKnownContextWindow(t *testing.T) {
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	srv.SetRuntimeDetectCache(runtimedetect.NewMemoryCache())
	req := httptest.NewRequest(http.MethodPost, "/internal/agents/alice/runtime-catalog", bytes.NewBufferString(
		`{"runtime":"claude-code","models":[{"id":"claude","contextWindow":0,"contextWindowKnown":true}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
