package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/messagebuffer"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/statechangestore"
	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// orphanTestStores bundles the memory backends so a test can seed them and then
// assert the finalizer reaped every per-agent row (kyber#565 AC-5).
type orphanTestStores struct {
	brief    briefstore.BriefStore
	token    tokenstore.TokenStore
	tokAccum tokenstore.Accumulator
	stAccum  statechangestore.Accumulator
	metrics  metricsstore.MetricsStore
	buffer   messagebuffer.MessageBuffer
}

func newOrphanTestStores() orphanTestStores {
	return orphanTestStores{
		brief:    briefstore.NewMemoryStore(),
		token:    tokenstore.NewMemoryStore(),
		tokAccum: tokenstore.NewMemoryAccumulator(),
		stAccum:  statechangestore.NewMemoryAccumulator(),
		metrics:  metricsstore.NewMemoryMetricsStore(),
		buffer:   messagebuffer.NewMemoryBuffer(),
	}
}

// seed writes one row per store for (ns, agent).
func (s orphanTestStores) seed(ctx context.Context, ns, agent string) {
	_ = s.brief.Put(ctx, agent, &briefstore.Brief{Version: 1, AgentName: agent})
	_ = s.token.Put(ctx, agent, &tokenreport.Snapshot{Tokens: tokenreport.Tokens{Used: 1}})
	_ = s.tokAccum.IncrBy(ctx, ns, agent, "claude-opus-4", tokenstore.TokenDelta{Input: 10})
	_ = s.stAccum.IncrBy(ctx, ns, agent, "working", 1)
	_ = s.metrics.AddPoint(ctx, metricsstore.ActivityKey(ns, agent, "working"), 100, 0.5)
	_ = s.buffer.Push(ctx, agent, messagebuffer.PendingMessage{Source: "telegram", Text: "hi"})
}

func (s orphanTestStores) wire(r *AgentReconciler) {
	r.BriefStore = s.brief
	r.TokenStore = s.token
	r.TokenAccumulator = s.tokAccum
	r.StateChangeAccumulator = s.stAccum
	r.MetricsStore = s.metrics
	r.MessageBuffer = s.buffer
}

// TestReconciler_Deletion_ReapsAllOrphanState covers kyber#565 AC-5 + AC-7: a
// confirmed delete that reaches the finalizer leaves zero orphaned identity
// state across every external store, while an unrelated agent's rows survive.
func TestReconciler_Deletion_ReapsAllOrphanState(t *testing.T) {
	ctx := context.Background()
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	stores := newOrphanTestStores()
	stores.wire(r)

	const ns = "test-orphan"
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	// Seed the agent being deleted plus an unrelated survivor in the same ns.
	stores.seed(ctx, ns, "dave")
	stores.seed(ctx, ns, "han")

	agent := newTestAgent("dave", ns)
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: ns}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: ns}

	// Bootstrap: finalizer + PVC + pod.
	reconcileN(t, r, req, 1)
	created := getAgent(t, k8sClient, agentKey)
	if !containsString(created.Finalizers, AgentFinalizer) {
		t.Fatal("finalizer not set before deletion")
	}

	if err := k8sClient.Delete(ctx, created); err != nil {
		t.Fatalf("deleting agent: %v", err)
	}
	// Drive the finalizer to completion (pod delete + requeue, then PVC/secrets/stores).
	reconcileN(t, r, req, 3)

	// AC-7: agent fully deleted / finalizer removed.
	final := &kyberv1.Agent{}
	if err := k8sClient.Get(ctx, agentKey, final); err == nil {
		if containsString(final.Finalizers, AgentFinalizer) {
			t.Error("finalizer still present after deletion reconcile")
		}
	} else if !errors.IsNotFound(err) {
		t.Fatalf("unexpected error getting agent: %v", err)
	}

	// AC-5: dave's rows are gone in every store.
	if b, _ := stores.brief.Get(ctx, "dave"); b != nil {
		t.Error("brief for dave must be reaped")
	}
	if snap, _ := stores.token.Get(ctx, "dave"); snap != nil {
		t.Error("token-usage for dave must be reaped")
	}
	if hasAgent(toAgentNames(getTokenAgents(ctx, t, stores.tokAccum, ns)), "dave") {
		t.Error("token accumulator rows for dave must be reaped")
	}
	if hasAgent(getStateAgents(ctx, t, stores.stAccum, ns), "dave") {
		t.Error("state-change accumulator rows for dave must be reaped")
	}
	if pts, _ := stores.metrics.RangeQuery(ctx, metricsstore.ActivityKey(ns, "dave", "working"), 0, 1<<62); len(pts) != 0 {
		t.Error("metrics time-series for dave must be reaped")
	}
	if msgs, _ := stores.buffer.Drain(ctx, "dave"); len(msgs) != 0 {
		t.Error("wake buffer for dave must be reaped")
	}

	// The unrelated agent han must be fully intact (no collateral cleanup).
	if b, _ := stores.brief.Get(ctx, "han"); b == nil {
		t.Error("han's brief must survive dave's deletion")
	}
	if !hasAgent(toAgentNames(getTokenAgents(ctx, t, stores.tokAccum, ns)), "han") {
		t.Error("han's token accumulator rows must survive")
	}
	if pts, _ := stores.metrics.RangeQuery(ctx, metricsstore.ActivityKey(ns, "han", "working"), 0, 1<<62); len(pts) != 1 {
		t.Error("han's metrics time-series must survive")
	}
}

// failingTokenStore wraps a TokenStore and always errors on Delete, simulating a
// durably-unreachable backend.
type failingTokenStore struct {
	tokenstore.TokenStore
}

func (failingTokenStore) Delete(context.Context, string) error {
	return errStoreUnreachable
}

var errStoreUnreachable = &storeErr{}

type storeErr struct{}

func (*storeErr) Error() string { return "simulated store outage" }

// TestReconciler_Deletion_OrphanCleanupGivesUp covers the bounded-retry-then-
// complete policy (Matt's locked decision): when a store is durably unreachable
// the finalizer requeues a bounded number of times, then completes deletion
// anyway while emitting an OrphanCleanupIncomplete Event naming what it couldn't
// reap — so an agent can never be permanently undeletable (kyber#565).
func TestReconciler_Deletion_OrphanCleanupGivesUp(t *testing.T) {
	ctx := context.Background()
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.Recorder = record.NewFakeRecorder(64)
	stores := newOrphanTestStores()
	stores.wire(r)
	// The token store is durably down.
	r.TokenStore = failingTokenStore{stores.token}

	const ns = "test-giveup"
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	stores.seed(ctx, ns, "dave")

	agent := newTestAgent("dave", ns)
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: ns}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: ns}

	reconcileN(t, r, req, 1)
	created := getAgent(t, k8sClient, agentKey)
	if err := k8sClient.Delete(ctx, created); err != nil {
		t.Fatalf("deleting agent: %v", err)
	}

	// Pod-delete requeue (1) + bounded cleanup retries (orphanCleanupMaxAttempts)
	// + the final give-up reconcile. Reconcile generously past the bound.
	reconcileN(t, r, req, orphanCleanupMaxAttempts+4)

	// The agent must NOT be stuck — finalizer removed / object gone.
	final := &kyberv1.Agent{}
	if err := k8sClient.Get(ctx, agentKey, final); err == nil {
		if containsString(final.Finalizers, AgentFinalizer) {
			t.Error("finalizer still present — a durable store outage must NOT wedge deletion forever")
		}
	} else if !errors.IsNotFound(err) {
		t.Fatalf("unexpected error getting agent: %v", err)
	}

	// And the give-up must be loud.
	if !sawEvent(t, r, "OrphanCleanupIncomplete") {
		t.Error("expected an OrphanCleanupIncomplete Event after bounded give-up")
	}
}

// --- small helpers to read back accumulator agents ---

func getTokenAgents(ctx context.Context, t *testing.T, a tokenstore.Accumulator, ns string) []tokenstore.AgentModelCounts {
	t.Helper()
	got, err := a.GetAll(ctx, ns)
	if err != nil {
		t.Fatalf("token GetAll: %v", err)
	}
	return got
}

func toAgentNames(cs []tokenstore.AgentModelCounts) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Agent)
	}
	return out
}

func getStateAgents(ctx context.Context, t *testing.T, a statechangestore.Accumulator, ns string) []string {
	t.Helper()
	got, err := a.GetAll(ctx, ns)
	if err != nil {
		t.Fatalf("state GetAll: %v", err)
	}
	out := make([]string, 0, len(got))
	for _, c := range got {
		out = append(out, c.Agent)
	}
	return out
}

func hasAgent(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
