package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/metrics"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// writeRatesFile writes a provider-rates.yaml fixture and returns its path.
func writeRatesFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "provider-rates.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rates: %v", err)
	}
	return path
}

// buildPricedMetricsServer wires a Tier-1 token metrics server with a real
// provider-rates file so the priced sentinel can be exercised end-to-end.
func buildPricedMetricsServer(t *testing.T, ratesPath string, ms metricsstore.MetricsStore, acc tokenstore.Accumulator) *httptest.Server {
	t.Helper()
	scheme := metricsTestScheme(t)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "han", Namespace: "kyber-system"}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	srv := &api.Server{
		K8sClient:        fakeClient,
		Namespace:        "kyber-system",
		APIKey:           "test-key",
		MetricsStore:     ms,
		TokenAccumulator: acc,
		MetricsConfig:    metrics.Config{TokenRatesPath: ratesPath},
	}
	return httptest.NewServer(srv.BuildHandler())
}

// TestMetricsTokens_PricedSentinel is the kyber#487 fail-loud wire-contract
// pin: the /api/v1/metrics/tokens response must carry a `priced` boolean so the
// PWA can render "—"/unpriced instead of a believable $0.0000.
//   - a model present in provider-rates → priced:true, non-zero cost
//   - a model absent from provider-rates → priced:false, cost 0 (the bug:
//     opus-4-8 has no rate)
//   - a model keyed by its dated release variant → priced:true via the
//     ComputeCostKnown normalization (haiku-4-5 fix)
func TestMetricsTokens_PricedSentinel(t *testing.T) {
	ratesPath := writeRatesFile(t, `
claude-sonnet-4-6:
  input: 3.0
  output: 15.0
  cache_read: 0.3
  cache_creation: 3.75
claude-haiku-4-5-20251001:
  input: 0.8
  output: 4.0
  cache_read: 0.08
`)
	ms := metricsstore.NewMemoryMetricsStore()
	acc := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()
	now := time.Now().Unix()
	const ns = "kyber-system"

	// priced model, unpriced model (opus-4-8 — the bug), dated-key model.
	type fixture struct {
		agent, model string
	}
	for _, f := range []fixture{
		{"sonny", "claude-sonnet-4-6"},
		{"oppy", "claude-opus-4-8"},
		{"hai", "claude-haiku-4-5"},
	} {
		_ = acc.IncrBy(ctx, ns, f.agent, f.model, tokenstore.TokenDelta{Input: 1_000_000})
		_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, f.agent, f.model, "input"), now-100, 1_000_000)
	}
	// sonny additionally carries output + cache_creation usage so the cost
	// includes both fixed components (output was previously dropped end-to-end;
	// cache_creation previously had no rate and priced at $0).
	_ = acc.IncrBy(ctx, ns, "sonny", "claude-sonnet-4-6",
		tokenstore.TokenDelta{Output: 200_000, CacheCreation: 100_000})
	_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, "sonny", "claude-sonnet-4-6", "output"), now-100, 200_000)
	_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, "sonny", "claude-sonnet-4-6", "cache_creation"), now-100, 100_000)

	ts := buildPricedMetricsServer(t, ratesPath, ms, acc)
	defer ts.Close()

	resp := getMetrics(t, ts, "/api/v1/metrics/tokens?start="+itoa(now-300)+"&end="+itoa(now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var rows []map[string]interface{}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}

	byAgent := map[string]map[string]interface{}{}
	for _, r := range rows {
		byAgent[r["agent"].(string)] = r
	}

	// Priced model: priced:true, cost = 1M input * $3/MTok
	// + 200K output * $15/MTok + 100K cache_creation * $3.75/MTok
	// = 3.0 + 3.0 + 0.375 = 6.375.
	if got := byAgent["sonny"]["priced"]; got != true {
		t.Errorf("sonnet priced = %v, want true", got)
	}
	if got := byAgent["sonny"]["costUsd"]; got != 6.375 {
		t.Errorf("sonnet costUsd = %v, want 6.375 (input+output+cache_creation)", got)
	}
	// The output count must round-trip into the response's tokens map.
	sonnyTokens, ok := byAgent["sonny"]["tokens"].(map[string]interface{})
	if !ok {
		t.Fatalf("sonny row missing tokens map: %v", byAgent["sonny"])
	}
	if got := sonnyTokens["output"]; got != 200_000.0 {
		t.Errorf("sonny tokens.output = %v, want 200000", got)
	}
	if got := sonnyTokens["cache_creation"]; got != 100_000.0 {
		t.Errorf("sonny tokens.cache_creation = %v, want 100000", got)
	}

	// Unpriced model (opus-4-8 — no rate): priced:false, cost 0 (NOT a
	// believable $0 — the frontend keys the badge off priced=false).
	if got := byAgent["oppy"]["priced"]; got != false {
		t.Errorf("opus-4-8 priced = %v, want false (no rate → unpriced)", got)
	}
	if got := byAgent["oppy"]["costUsd"]; got != 0.0 {
		t.Errorf("opus-4-8 costUsd = %v, want 0", got)
	}

	// Dated-key model: priced:true via normalization, cost = 1M * $0.8 = 0.8.
	if got := byAgent["hai"]["priced"]; got != true {
		t.Errorf("haiku-4-5 priced = %v, want true (dated key must resolve)", got)
	}
	if got := byAgent["hai"]["costUsd"]; got != 0.8 {
		t.Errorf("haiku-4-5 costUsd = %v, want 0.8", got)
	}
}
