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

func TestLegacyMetricsTokensRouteRemoved(t *testing.T) {
	ts := buildMetricsServer(t, nil, nil, nil)
	defer ts.Close()
	resp := getMetrics(t, ts, "/api/v1/metrics/tokens")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("legacy token-cost route status = %d, want 404", resp.StatusCode)
	}
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

// itoa converts int64 to string for URL params.
func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
