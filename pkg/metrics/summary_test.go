package metrics

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	if err := kyberv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func agentObjects(agents []kyberv1.Agent) []runtime.Object {
	out := make([]runtime.Object, len(agents))
	for i := range agents {
		out[i] = &agents[i]
	}
	return out
}

func TestGetFleetSummary(t *testing.T) {
	scheme := newTestScheme(t)

	agents := []kyberv1.Agent{
		{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "kyber"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning, Activity: &kyberv1.ActivityStatus{State: "working"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a2", Namespace: "kyber"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning, Activity: &kyberv1.ActivityStatus{State: "idle"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a3", Namespace: "kyber"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseStopped}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a4", Namespace: "kyber"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseStarting}},
	}

	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agentObjects(agents)...).Build()

	summary, err := GetFleetSummary(context.Background(), k8s, "kyber")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.Total != 4 {
		t.Errorf("Total: got %d, want 4", summary.Total)
	}
	if summary.Working != 1 {
		t.Errorf("Working: got %d, want 1", summary.Working)
	}
	if summary.Idle != 2 { // idle-state Running + Starting
		t.Errorf("Idle: got %d, want 2", summary.Idle)
	}
	if summary.Offline != 1 {
		t.Errorf("Offline: got %d, want 1", summary.Offline)
	}
}

func TestGetLastActive_Stale(t *testing.T) {
	scheme := newTestScheme(t)
	staleTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	freshTime := metav1.NewTime(time.Now().Add(-30 * time.Second))

	agents := []kyberv1.Agent{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "kyber"},
			Status:     kyberv1.AgentStatus{Activity: &kyberv1.ActivityStatus{LastHeartbeatAt: &freshTime}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: "kyber"},
			Status:     kyberv1.AgentStatus{Activity: &kyberv1.ActivityStatus{LastHeartbeatAt: &staleTime}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "nobeat", Namespace: "kyber"},
			Status:     kyberv1.AgentStatus{},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agentObjects(agents)...).Build()

	cfg := Config{StalenessThresholdSeconds: 300}
	entries, err := GetLastActive(context.Background(), k8s, "kyber", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	byName := map[string]AgentLastActive{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	if byName["fresh"].Stale {
		t.Errorf("fresh agent should not be stale")
	}
	if !byName["stale"].Stale {
		t.Errorf("stale agent should be stale")
	}
	if !byName["nobeat"].Stale {
		t.Errorf("agent with no heartbeat should be stale")
	}
}

func TestRateTable_ComputeCostUSD(t *testing.T) {
	table := RateTable{
		"claude-sonnet-4-6": ProviderRates{Input: 3.0, Output: 15.0, CacheRead: 0.3},
	}

	counts := map[string]float64{"input": 1_000_000, "output": 500_000, "cache_read": 2_000_000}
	cost := table.ComputeCostUSD("claude-sonnet-4-6", counts)
	// 3.0 + 7.5 + 0.6 = 11.1
	if cost != 11.1 {
		t.Errorf("ComputeCostUSD = %v, want 11.1", cost)
	}

	if table.ComputeCostUSD("unknown-model", counts) != 0 {
		t.Errorf("expected 0 for unknown model")
	}
}
