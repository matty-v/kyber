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
	if got["qwen"] != 65536 {
		t.Errorf("context window = %d, want the served 65536", got["qwen"])
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
	if len(got) != 0 {
		t.Errorf("windows = %v, want none", got)
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

	models, err := LoadHermesCatalog(providerPath, metadataPath, []string{"kyber-endpoint"}, map[string]int64{"qwen3.6-35b-a3b": 65536}, 100)
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

	models, err := LoadHermesCatalog(providerPath, metadataPath, []string{"openrouter"}, map[string]int64{"vendor/a": 65536}, 100)
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

	snap, err := ParseHermesLatest(logPath, metadataPath, map[string]int64{"qwen3.6-35b-a3b": 65536})
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

// Per-model, not one value stamped on all of them: an endpoint can serve
// several models, and budgeting a small one against a big one's window is the
// guess this code refuses to make when nothing is published.
func TestEndpointContextWindowIsPerModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"big","meta":{"n_ctx":131072}},{"id":"small","meta":{"n_ctx":8192}}]}`))
	}))
	defer srv.Close()

	got, err := EndpointContextWindow(context.Background(), srv.Client(), srv.URL+"/v1", "")
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if got["big"] != 131072 || got["small"] != 8192 {
		t.Errorf("windows = %v, want each model its own", got)
	}
}

// The agent log is durable across a switch onto an endpoint. An OpenRouter
// line left in it must not be budgeted against the endpoint's window and
// asserted as known — a 131k model reported against 65k reads as twice the
// usage it really is.
func TestParseHermesLatestDoesNotApplyTheEndpointWindowToOpenRouterLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	line := `2026-09-22 05:00:00,000 INFO [sess] agent.conversation_loop: API call #1: model=vendor/a provider=openrouter in=1000 out=10 total=1010 latency=1.0s cache=0/1000 (0%)` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	snap, err := ParseHermesLatest(logPath, metadataPath, map[string]int64{"vendor/a": 65536})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.ContextWindowKnown || snap.Tokens.Limit != 0 {
		t.Errorf("limit = %d known = %v; an OpenRouter line must not borrow the endpoint's window",
			snap.Tokens.Limit, snap.ContextWindowKnown)
	}
}

// Hermes keys its provider cache by "custom:<base_url>" for a provider
// configured with an explicit base_url — NOT by the provider's name from
// config.yaml. Looking up the name found nothing, so an endpoint agent posted
// an EMPTY catalog, which the control plane rejects outright: the model picker
// stayed empty and the reporter retried until it backed off to hourly.
// Verified against a live agent, whose cache held only
// "custom:https://llm.voget.io/v1".
func TestLoadHermesCatalogFindsTheCustomProviderKey(t *testing.T) {
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider_models_cache.json")
	metadataPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(providerPath,
		[]byte(`{"openai-api":{"models":["gpt-x"]},"custom:https://llm.voget.io/v1":{"models":["qwen3.6-35b-a3b"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	keys := []string{"kyber-endpoint", HermesCustomProviderKey("https://llm.voget.io/v1")}
	models, err := LoadHermesCatalog(providerPath, metadataPath, keys,
		map[string]int64{"qwen3.6-35b-a3b": 65536}, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(models) != 1 || models[0].ID != "qwen3.6-35b-a3b" {
		t.Fatalf("models = %+v, want the endpoint's model found under the custom key", models)
	}
	if !models[0].ContextWindowKnown {
		t.Error("window must be known, or the control plane rejects the catalog")
	}
}

// The first key that is actually present wins, so a named provider is still
// preferred when Hermes cached it that way.
func TestLoadHermesCatalogPrefersTheFirstPresentProviderKey(t *testing.T) {
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider_models_cache.json")
	metadataPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(providerPath,
		[]byte(`{"kyber-endpoint":{"models":["named"]},"custom:https://x/v1":{"models":["custom"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	models, err := LoadHermesCatalog(providerPath, metadataPath,
		[]string{"kyber-endpoint", HermesCustomProviderKey("https://x/v1")},
		map[string]int64{"named": 4096, "custom": 8192}, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(models) != 1 || models[0].ID != "named" {
		t.Fatalf("models = %+v, want the named provider's entry", models)
	}
}
