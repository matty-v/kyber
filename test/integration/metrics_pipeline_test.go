//go:build integration

package integration

// metrics_pipeline_test.go — Regression gate for the #356/#358/#360 arc.
//
// What this test protects against: kyber#356 and kyber#358 both shipped on
// green CI and closed before live verification. Each fix exercised its own
// component in isolation; nothing in CI walked the full cross-component
// chain (sidecar snapshot → CP /internal/.../status handler → MetricsStore
// → /api/v1/metrics/activity). The result was two iterations of "panels
// stay dark in prod" before kyber#360 caught the third stacked cause.
//
// What this test covers: end-to-end pipeline using the real Redis-backed
// MetricsStore and the real CP route handlers (InternalServer's
// handleStatusSnapshot for ingest, public Server's handleMetricsActivity
// for read). The sidecar's snapshot wire shape is replicated verbatim —
// the JSON body posted to /internal/agents/{name}/status is byte-for-byte
// what cmd/status-sidecar/main.go's postMetricsSnapshot emits, so a
// breaking change to either side trips this test.
//
// What this test does NOT cover: the sidecar-internal forwardHandler hop
// (event POST → forwardHandler → control-plane URL) is exercised by
// cmd/status-sidecar/main_test.go's TestForwarder_RoutesAllThreePaths
// and TestForwarder_ForwardsLocalhostEventToControlPlane. Keeping the
// integration test at the cross-component boundary (sidecar→CP wire)
// avoids importing cmd/* internals here and matches the integration
// pattern already established in test/integration/.
//
// Branch protection is the other half of this gate: this test must be in
// the required-green check set for kyber's main branch, not just running
// in CI. CI passing a test that nobody waits for is what shipped #356 and
// #358; the gate is the merge block, not the test file's existence.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/statechangestore"
)

// TestMetricsPipeline_SnapshotToActivityRead drives the full cross-component
// pipeline that #356/#358 missed: a synthetic snapshot POST (the byte-for-
// byte payload the sidecar emits in cmd/status-sidecar/main.go's
// postMetricsSnapshot) lands in the CP InternalServer, fans out to the
// real Redis MetricsStore, and is then read back via the public
// /api/v1/metrics/activity route.
//
// Two POSTs are required because handleStatusSnapshot stores deltas vs
// the previous snapshot — the first sets the baseline, the second
// produces a non-zero point that the activity panel can render.
func TestMetricsPipeline_SnapshotToActivityRead(t *testing.T) {
	ctx := context.Background()
	agentName := fmt.Sprintf("metrics-pipeline-%d", time.Now().UnixNano())

	// Clean up any leftover Redis keys from a previous run. Sorted-set
	// keys follow the canonical metricsstore.ActivityKey schema.
	for _, state := range []string{"working", "idle", "paused"} {
		cleanRedisKey(t, metricsstore.ActivityKey(testNamespace, agentName, state))
	}

	// Real Redis-backed MetricsStore — same backend production uses.
	metricsStore := metricsstore.NewRedisMetricsStore(sharedRDB)
	stateChangeAcc := statechangestore.NewMemoryAccumulator()

	k8s := fakeClientWithAgent(t, agentName)

	// In-process InternalServer. BriefStore is required by NewInternalServer
	// but unused by handleStatusSnapshot; a memory store keeps the cost zero.
	//
	// WithKubeClient is plumbed BOTH for the OAuth rotation endpoint AND —
	// non-obviously — to set InternalServer.namespace, which handleStatusSnapshot
	// uses when composing metricsstore.ActivityKey on writes. Without it,
	// snapshots land under key prefix "ts:activity::<agent>:..." while the
	// public Server's activityFromMetricsStore reads "ts:activity:kyber-system:
	// <agent>:..." (it sources namespace from Server.Namespace) — same Redis,
	// different keys, regression test reports [] forever. Production wires this
	// at cmd/control-plane/main.go (NewInternalServer with WithKubeClient), so
	// the integration test must mirror it to actually exercise the prod
	// assembly. The dual purpose of WithKubeClient is a footgun worth a
	// dedicated WithNamespace option in a follow-up; out of scope here.
	internalSrv := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithMetricsStore(metricsStore),
		api.WithStateChangeAccumulator(stateChangeAcc),
		api.WithKubeClient(k8s, testNamespace),
	)
	internalTS := httptest.NewServer(internalSrv.Handler())
	defer internalTS.Close()

	// Public Server with the same MetricsStore + a fake k8s client that
	// lists the same agent. activityFromMetricsStore enumerates agents
	// from the CRD list, so the agent must exist in K8s state for the
	// read path to find its keys.
	publicSrv := &api.Server{
		K8sClient:    k8s,
		APIKey:       testAPIKey,
		Namespace:    testNamespace,
		MetricsStore: metricsStore,
	}
	publicTS := httptest.NewServer(publicSrv.BuildHandler())
	defer publicTS.Close()

	// First snapshot — baseline. handleStatusSnapshot stores deltas vs the
	// previous snapshot, so this POST establishes the prior value (no
	// time-series point is written because delta == 0).
	postSnapshot(t, ctx, internalTS.URL, agentName, map[string]float64{"working": 10}, time.Now().Add(-30*time.Second))

	// Second snapshot — produces a positive delta (15 seconds) and a stored point.
	postSnapshot(t, ctx, internalTS.URL, agentName, map[string]float64{"working": 25}, time.Now())

	// GET /api/v1/metrics/activity over the last 5 minutes. Must return
	// at least one series with at least one point — the regression these
	// tests guard against is "[]" coming back here despite ingest looking
	// successful upstream.
	end := time.Now().Unix()
	start := end - 5*60
	url := fmt.Sprintf("%s/api/v1/metrics/activity?start=%d&end=%d", publicTS.URL, start, end)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET activity: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET activity status: got %d, want 200", resp.StatusCode)
	}

	var series []struct {
		Labels map[string]string `json:"labels"`
		Points []struct {
			Timestamp int64   `json:"ts"`
			Value     float64 `json:"v"`
		} `json:"points"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&series); err != nil {
		t.Fatalf("decode activity response: %v", err)
	}
	if len(series) == 0 {
		t.Fatal("activity returned []; expected at least one series — this is the #356/#358 regression")
	}
	var totalPoints int
	for _, s := range series {
		totalPoints += len(s.Points)
	}
	if totalPoints == 0 {
		t.Fatalf("activity returned %d series but zero points across all of them; expected ≥1 stored point", len(series))
	}
}

// TestMetricsPipeline_UnknownStateRoundTripsToActivityRead is the
// integration-tier regression gate for kyber#360 Cause F. The runtime
// emits 'unknown' (pkg/tokenreport/activity.go's ActivityUnknown constant)
// when DetectActivity errors; before this fix the CP 400'd on the entire
// snapshot batch and the data was silently lost. This test asserts the
// full pipeline — sidecar wire shape → CP /internal/.../status →
// MetricsStore → public /api/v1/metrics/activity — round-trips an
// 'unknown' series end-to-end.
func TestMetricsPipeline_UnknownStateRoundTripsToActivityRead(t *testing.T) {
	ctx := context.Background()
	agentName := fmt.Sprintf("unknown-state-%d", time.Now().UnixNano())

	for _, state := range []string{"working", "idle", "paused", "unknown"} {
		cleanRedisKey(t, metricsstore.ActivityKey(testNamespace, agentName, state))
	}

	metricsStore := metricsstore.NewRedisMetricsStore(sharedRDB)
	stateChangeAcc := statechangestore.NewMemoryAccumulator()
	k8s := fakeClientWithAgent(t, agentName)
	internalSrv := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithMetricsStore(metricsStore),
		api.WithStateChangeAccumulator(stateChangeAcc),
		api.WithKubeClient(k8s, testNamespace),
	)
	internalTS := httptest.NewServer(internalSrv.Handler())
	defer internalTS.Close()

	publicSrv := &api.Server{
		K8sClient:    k8s,
		APIKey:       testAPIKey,
		Namespace:    testNamespace,
		MetricsStore: metricsStore,
	}
	publicTS := httptest.NewServer(publicSrv.BuildHandler())
	defer publicTS.Close()

	// Baseline + a snapshot containing both 'unknown' (the new vocab) and
	// 'working' (long-accepted) — the live cluster's worst-case payload
	// shape after a detector-error blip.
	postSnapshot(t, ctx, internalTS.URL, agentName, map[string]float64{"unknown": 0, "working": 0}, time.Now().Add(-30*time.Second))
	postSnapshot(t, ctx, internalTS.URL, agentName, map[string]float64{"unknown": 5, "working": 20}, time.Now())

	end := time.Now().Unix()
	start := end - 5*60
	url := fmt.Sprintf("%s/api/v1/metrics/activity?start=%d&end=%d", publicTS.URL, start, end)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET activity: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET activity status: got %d, want 200", resp.StatusCode)
	}

	var series []struct {
		Labels map[string]string `json:"labels"`
		Points []struct {
			Timestamp int64   `json:"ts"`
			Value     float64 `json:"v"`
		} `json:"points"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&series); err != nil {
		t.Fatalf("decode activity response: %v", err)
	}

	sawUnknown := false
	for _, s := range series {
		if s.Labels["state"] == "unknown" && len(s.Points) > 0 {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Fatalf("activity series missing non-empty 'unknown' entry — Cause F regression: got %+v", series)
	}
}

// postSnapshot POSTs a status snapshot to InternalServer. Wire shape
// mirrors cmd/status-sidecar/main.go's postMetricsSnapshot; any drift on
// either side will be caught by this test failing to round-trip the data.
func postSnapshot(t *testing.T, ctx context.Context, baseURL, agentName string, stateSecs map[string]float64, at time.Time) {
	t.Helper()
	payload := map[string]any{
		"activity_state_seconds": stateSecs,
		"at":                     at.UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	url := fmt.Sprintf("%s/internal/agents/%s/status", baseURL, agentName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build POST: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST snapshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot POST status: got %d, want 200", resp.StatusCode)
	}
}

// fakeClientWithAgent builds a controller-runtime fake client that lists
// one Agent in testNamespace. activityFromMetricsStore reads the agent
// list and only queries MetricsStore for agents it finds — without an
// agent in K8s state, the read path returns [] regardless of whether
// ingest worked.
func fakeClientWithAgent(t *testing.T, name string) client.Client {
	t.Helper()
	scheme := newTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
}
