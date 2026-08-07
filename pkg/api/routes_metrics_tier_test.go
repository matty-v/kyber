package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/statechangestore"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

func metricsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := kyberv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func buildMetricsServer(t *testing.T, ms metricsstore.MetricsStore, ns metricsstore.NodeStore, sc statechangestore.Accumulator) *httptest.Server {
	t.Helper()
	scheme := metricsTestScheme(t)

	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "han",
			Namespace: "kyber-system",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent).
		Build()

	srv := &api.Server{
		K8sClient:              fakeClient,
		Namespace:              "kyber-system",
		APIKey:                 "test-key",
		MetricsStore:           ms,
		NodeStore:              ns,
		StateChangeAccumulator: sc,
	}
	return httptest.NewServer(srv.BuildHandler())
}

func getMetrics(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// TestTierPreference_StateChanges_RedisOverPrometheus verifies that when
// StateChangeAccumulator has data, the state-changes handler returns it
// without querying Prometheus (which is not configured here).
func TestTierPreference_StateChanges_RedisOverPrometheus(t *testing.T) {
	sc := statechangestore.NewMemoryAccumulator()
	ctx := context.Background()
	_ = sc.IncrBy(ctx, "kyber-system", "han", "working", 5)

	ts := buildMetricsServer(t, nil, nil, sc)
	defer ts.Close()

	now := time.Now().Unix()
	resp := getMetrics(t, ts, "/api/v1/metrics/state-changes?start="+
		itoa(now-3600)+"&end="+itoa(now))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var entries []map[string]interface{}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d: %s", len(entries), body)
	}
	if entries[0]["agent"] != "han" || entries[0]["toState"] != "working" {
		t.Errorf("unexpected entry: %v", entries[0])
	}
}

// TestTierPreference_Nodes_RedisOverPrometheus verifies that when NodeStore
// has data, the nodes handler returns it.
func TestTierPreference_Nodes_RedisOverPrometheus(t *testing.T) {
	ns := metricsstore.NewMemoryNodeStore()
	ctx := context.Background()
	_ = ns.PutNode(ctx, "kyber-system", "node-1", metricsstore.NodeSample{
		CPUPercent:     55.5,
		MemUsedBytes:   2 << 30,
		MemTotalBytes:  8 << 30,
		DiskUsedBytes:  10 << 30,
		DiskTotalBytes: 100 << 30,
	})

	ts := buildMetricsServer(t, nil, ns, nil)
	defer ts.Close()

	resp := getMetrics(t, ts, "/api/v1/metrics/nodes")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var nodes []map[string]interface{}
	if err := json.Unmarshal(body, &nodes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(nodes))
	}
	if nodes[0]["node"] != "node-1" {
		t.Errorf("node = %v, want node-1", nodes[0]["node"])
	}
	if nodes[0]["cpuPercent"] != 55.5 {
		t.Errorf("cpuPercent = %v, want 55.5", nodes[0]["cpuPercent"])
	}
}

// TestTierPreference_Activity_IncludesUnknownState is the unit-side
// regression gate for kyber#368 Cause G. The CP now accepts 'unknown' on
// the write path (kyber#360 Cause F) but the read path's enumerator must
// also include it — otherwise points are stored and silently invisible to
// the PWA, which is the exact failure mode CI caught on the integration
// test. Asserts the activity handler returns a series labelled
// state=unknown when one exists in MetricsStore.
func TestTierPreference_Activity_IncludesUnknownState(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	ctx := context.Background()
	now := time.Now().Unix()
	_ = ms.AddPoint(ctx, metricsstore.ActivityKey("kyber-system", "han", "unknown"), now-30, 5.0)

	ts := buildMetricsServer(t, ms, nil, nil)
	defer ts.Close()

	resp := getMetrics(t, ts, "/api/v1/metrics/activity?start="+
		itoa(now-3600)+"&end="+itoa(now))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var series []map[string]interface{}
	if err := json.Unmarshal(body, &series); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	sawUnknown := false
	for _, s := range series {
		labels, _ := s["labels"].(map[string]interface{})
		if labels != nil && labels["state"] == "unknown" {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Fatalf("activity series missing 'unknown' entry — Cause G regression: got %s", body)
	}
}

// TestTierPreference_Activity_RedisOverPrometheus verifies that when MetricsStore
// has data, the activity handler returns it.
func TestTierPreference_Activity_RedisOverPrometheus(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	ctx := context.Background()
	now := time.Now().Unix()
	_ = ms.AddPoint(ctx, metricsstore.ActivityKey("kyber-system", "han", "working"), now-30, 10.0)

	ts := buildMetricsServer(t, ms, nil, nil)
	defer ts.Close()

	resp := getMetrics(t, ts, "/api/v1/metrics/activity?start="+
		itoa(now-3600)+"&end="+itoa(now))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var series []map[string]interface{}
	if err := json.Unmarshal(body, &series); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(series) == 0 {
		t.Fatalf("want at least 1 series, got 0 (body: %s)", body)
	}
}

// TestTierPreference_WorkingTime_RedisOverPrometheus verifies that when MetricsStore
// has working-state data, the working-time handler returns it without querying Prometheus.
func TestTierPreference_WorkingTime_RedisOverPrometheus(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	ctx := context.Background()
	now := time.Now().Unix()
	// Seed a working-state delta point for agent "han".
	_ = ms.AddPoint(ctx, metricsstore.ActivityKey("kyber-system", "han", "working"), now-30, 15.0)

	ts := buildMetricsServer(t, ms, nil, nil)
	defer ts.Close()

	resp := getMetrics(t, ts, "/api/v1/metrics/working-time?start="+
		itoa(now-3600)+"&end="+itoa(now))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var series []map[string]interface{}
	if err := json.Unmarshal(body, &series); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if len(series) == 0 {
		t.Fatalf("want at least 1 series, got 0 (body: %s)", body)
	}
	// Value is in hours (seconds / 3600); 15s → 15/3600 ≈ 0.00417.
	if series[0]["labels"] == nil {
		t.Errorf("missing labels on series: %v", series[0])
	}
}

// buildTokenMetricsServer builds a metrics test server wired with both a
// MetricsStore (Tier-1 windowed token series) and a TokenAccumulator
// (pair enumerator / all-time view), mirroring the production co-wiring in
// cmd/control-plane/main.go where both are set together when Redis is present.
func buildTokenMetricsServer(t *testing.T, ms metricsstore.MetricsStore, acc tokenstore.Accumulator) *httptest.Server {
	t.Helper()
	scheme := metricsTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "han", Namespace: "kyber-system"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	srv := &api.Server{
		K8sClient:        fakeClient,
		Namespace:        "kyber-system",
		APIKey:           "test-key",
		MetricsStore:     ms,
		TokenAccumulator: acc,
	}
	return httptest.NewServer(srv.BuildHandler())
}

// tokenRows fetches /api/v1/metrics/tokens for [start,end] and decodes the rows.
func tokenRows(t *testing.T, ts *httptest.Server, start, end int64) []map[string]interface{} {
	t.Helper()
	resp := getMetrics(t, ts, "/api/v1/metrics/tokens?start="+itoa(start)+"&end="+itoa(end))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var rows []map[string]interface{}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	return rows
}

// tokenInput returns the "input" token count for the named agent across the
// returned rows (0 if the agent is absent).
func tokenInput(t *testing.T, rows []map[string]interface{}, agent string) float64 {
	t.Helper()
	for _, row := range rows {
		if row["agent"] != agent {
			continue
		}
		tokens, ok := row["tokens"].(map[string]interface{})
		if !ok {
			t.Fatalf("row missing tokens map: %v", row)
		}
		v, _ := tokens["input"].(float64)
		return v
	}
	return 0
}

// TestMetricsTokens_WindowHonored is the core regression gate for kyber#428.
// The Token Usage card silently served all-time totals for every time filter
// because handleMetricsTokens read the all-time accumulator and ignored
// start/end. Under Option A the handler must read a windowed Redis token time
// series, so two distinct windows over data that differs across them return
// DISTINCT results, and a window entirely outside the data's range returns no
// current data (not the all-time set).
func TestMetricsTokens_WindowHonored(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	acc := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()
	now := time.Now().Unix()

	const ns, agent, model = "kyber-system", "han", "claude-sonnet-4-6"

	// Accumulator enumerates the (agent, model) pair (all-time / enumerator role).
	_ = acc.IncrBy(ctx, ns, agent, model, tokenstore.TokenDelta{Input: 1500})

	// Windowed token series: an old delta (30 min ago) and a recent delta (100s ago).
	_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, agent, model, "input"), now-1800, 1000)
	_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, agent, model, "input"), now-100, 500)

	ts := buildTokenMetricsServer(t, ms, acc)
	defer ts.Close()

	// Window A: last 5 minutes → only the now-100 point → input 500.
	inputA := tokenInput(t, tokenRows(t, ts, now-300, now), agent)
	// Window B: last hour → both points → input 1500.
	inputB := tokenInput(t, tokenRows(t, ts, now-3600, now), agent)

	if inputA == inputB {
		t.Fatalf("windowed token results must differ across distinct windows, both = %v (window ignored — the #428 bug)", inputA)
	}
	if inputA != 500 {
		t.Errorf("window A (5m) input = %v, want 500", inputA)
	}
	if inputB != 1500 {
		t.Errorf("window B (1h) input = %v, want 1500", inputB)
	}

	// Out-of-range window: 30 days in the past, 1h slice → no current data.
	// Must NOT fall through to the all-time set (authoritative-empty).
	past := tokenRows(t, ts, now-30*86400-3600, now-30*86400)
	if len(past) != 0 {
		t.Fatalf("out-of-range window must return no current data, got %d rows: %v", len(past), past)
	}
}

// TestMetricsTokens_AuthoritativeEmpty pins the subtle correctness point from
// the design: when MetricsStore is configured and the accumulator shows token
// data has ever existed, a window that yields zero points returns an empty
// result rather than falling through to Tier-2 and resurrecting all-time data.
func TestMetricsTokens_AuthoritativeEmpty(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	acc := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()
	now := time.Now().Unix()

	const ns, agent, model = "kyber-system", "han", "claude-sonnet-4-6"

	// Accumulator non-empty (token data has existed) but the series has a single
	// old point well outside the queried window.
	_ = acc.IncrBy(ctx, ns, agent, model, tokenstore.TokenDelta{Input: 1000})
	_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, agent, model, "input"), now-2*86400, 1000)

	ts := buildTokenMetricsServer(t, ms, acc)
	defer ts.Close()

	rows := tokenRows(t, ts, now-3600, now)
	if len(rows) != 0 {
		t.Fatalf("authoritative-empty: window with no points must return [], got %d rows: %v", len(rows), rows)
	}
}

// TestTierPreference_TokenUsage_RedisOverPrometheus verifies that when the
// windowed Redis token series has data, the tokens handler serves it without
// querying Prometheus (which is not configured here). Updated for kyber#428:
// Tier-1 is now the windowed MetricsStore series (enumerated via the
// accumulator), not the all-time accumulator counts.
func TestTierPreference_TokenUsage_RedisOverPrometheus(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	acc := tokenstore.NewMemoryAccumulator()
	ctx := context.Background()
	now := time.Now().Unix()

	const ns, agent, model = "kyber-system", "han", "claude-sonnet-4-6"
	_ = acc.IncrBy(ctx, ns, agent, model,
		tokenstore.TokenDelta{Input: 1000, CacheCreation: 200, CacheRead: 50})
	_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, agent, model, "input"), now-30, 1000)
	_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, agent, model, "cache_creation"), now-30, 200)
	_ = ms.AddPoint(ctx, metricsstore.TokenUsageKey(ns, agent, model, "cache_read"), now-30, 50)

	ts := buildTokenMetricsServer(t, ms, acc)
	defer ts.Close()

	rows := tokenRows(t, ts, now-3600, now)
	if len(rows) == 0 {
		t.Fatalf("want at least 1 token row, got 0")
	}
	if rows[0]["agent"] != "han" {
		t.Errorf("agent = %v, want han", rows[0]["agent"])
	}
	if got := tokenInput(t, rows, agent); got != 1000 {
		t.Errorf("input = %v, want 1000", got)
	}
}

// itoa converts int64 to string for URL params.
func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
