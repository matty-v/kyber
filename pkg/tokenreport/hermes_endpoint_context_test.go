package tokenreport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// llama.cpp publishes the SERVED window as data[].meta.n_ctx. n_ctx_train is
// what the model was trained for and is routinely far larger — reporting that
// would tell the UI the agent has four times the room it does.
func TestEndpointContextWindowPrefersServedOverTrained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer endpoint-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen","meta":{"n_ctx":65536,"n_ctx_train":262144}}]}`))
	}))
	defer srv.Close()

	got, err := EndpointContextWindow(context.Background(), srv.Client(), srv.URL+"/v1", "endpoint-key")
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if got != 65536 {
		t.Errorf("context window = %d, want the served 65536", got)
	}
}

// An endpoint that publishes nothing must yield 0, not a guess. A wrong window
// is worse than an honest unknown: it silently mis-sizes the budget.
func TestEndpointContextWindowReturnsZeroWhenUnpublished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
	}))
	defer srv.Close()

	got, err := EndpointContextWindow(context.Background(), srv.Client(), srv.URL+"/v1", "")
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if got != 0 {
		t.Errorf("context window = %d, want 0", got)
	}
}

func TestEndpointContextWindowSurfacesHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := EndpointContextWindow(context.Background(), srv.Client(), srv.URL+"/v1", "bad"); err == nil {
		t.Fatal("a 401 must be reported, not treated as an unknown window")
	}
}

// The regression this fixes: Hermes declares RequireCatalogContext, so the
// control plane rejects the WHOLE catalog when any model's window is unknown.
// A self-hosted model has no OpenRouter metadata, so every catalog was
// rejected — the picker stayed empty and the reporter retried forever.
func TestLoadHermesCatalogUsesTheEndpointWindowWhenMetadataHasNone(t *testing.T) {
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider_models_cache.json")
	metadataPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(providerPath, []byte(`{"kyber-endpoint":{"models":["qwen3.6-35b-a3b"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	models, err := LoadHermesCatalog(providerPath, metadataPath, "kyber-endpoint", 65536, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	if !models[0].ContextWindowKnown {
		t.Error("the window must be reported as known, or the control plane rejects the whole catalog")
	}
	if models[0].ContextWindow != 65536 {
		t.Errorf("context window = %d, want 65536", models[0].ContextWindow)
	}
}

// OpenRouter metadata stays authoritative where it exists — the endpoint value
// is a fallback, not an override.
func TestLoadHermesCatalogPrefersMetadataOverTheEndpointWindow(t *testing.T) {
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider_models_cache.json")
	metadataPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(providerPath, []byte(`{"openrouter":{"models":["vendor/a"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(`{"vendor/a":{"name":"Vendor A","context_length":128000}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	models, err := LoadHermesCatalog(providerPath, metadataPath, "openrouter", 65536, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if models[0].ContextWindow != 128000 {
		t.Errorf("context window = %d, want the metadata's 128000", models[0].ContextWindow)
	}
}

// Without this the token snapshot carried limit 0 and the UI showed
// "Context: -" for a self-hosted model.
func TestParseHermesLatestUsesTheEndpointWindow(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	line := `2026-09-22 05:00:00,000 INFO [sess] agent.conversation_loop: API call #1: model=qwen3.6-35b-a3b provider=custom in=18349 out=77 total=18426 latency=10.0s cache=12291/18349 (67%)` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	snap, err := ParseHermesLatest(logPath, metadataPath, 65536)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap == nil {
		t.Fatal("no snapshot produced")
	}
	if !snap.ContextWindowKnown || snap.Tokens.Limit != 65536 {
		t.Errorf("limit = %d known = %v, want 65536/true", snap.Tokens.Limit, snap.ContextWindowKnown)
	}
	if snap.Percentage <= 0 {
		t.Errorf("percentage = %v, want a real budget", snap.Percentage)
	}
}
