package agent

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// A Running agent whose pod carries a RECENT DeletionTimestamp is mid
// graceful roll (set-model, secret update, operator restart) — pods
// never acquire a DeletionTimestamp by crashing. Classifying this as
// PodDied produced a false Failed flash + AgentCrashed warning +
// restartCount bump when a reconcile raced the roll's own status patch
// (reproduced live on the canary 2026-08-22, agent "biggs"). The
// classifier must wait instead.

func deletionTimestampAgo(deletedAgo time.Duration) *metav1.Time {
	ts := metav1.NewTime(time.Now().Add(-deletedAgo))
	return &ts
}

func TestClassifyEvent_Running_RecentlyTerminatingPod_Waits(t *testing.T) {
	r := reconcilerWithRuntimeImage(driftDesiredImage)
	pod := podWithAgentSpecImage("agent-dave", driftDesiredImage)
	pod.DeletionTimestamp = deletionTimestampAgo(5*time.Second)

	event, err := r.classifyEvent(context.Background(), runningAgentOnRuntime(), pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != "" {
		t.Errorf("recently terminating pod classified %q, want no event (wait for the roll's own transition)", event)
	}
}

func TestClassifyEvent_Running_StuckTerminatingPod_StillRecovers(t *testing.T) {
	// Dead-node case: a pod stuck Terminating past the 60s bound must
	// keep flowing into the existing dead-pod recovery (PodDied →
	// Failed → auto-restart), not wait forever.
	r := reconcilerWithRuntimeImage(driftDesiredImage)
	pod := podWithAgentSpecImage("agent-dave", driftDesiredImage)
	pod.DeletionTimestamp = deletionTimestampAgo(2*time.Minute)

	event, err := r.classifyEvent(context.Background(), runningAgentOnRuntime(), pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventPodDied {
		t.Errorf("stuck terminating pod classified %q, want %q (dead-node recovery preserved)", event, EventPodDied)
	}
}

func TestClassifyEvent_Running_TerminatingWithDesiredRestarting_RoutesRestart(t *testing.T) {
	// Operator intent recorded in spec still wins over the wait guard.
	r := reconcilerWithRuntimeImage(driftDesiredImage)
	pod := podWithAgentSpecImage("agent-dave", driftDesiredImage)
	pod.DeletionTimestamp = deletionTimestampAgo(5*time.Second)
	agent := runningAgentOnRuntime()
	agent.Spec.DesiredPhase = "Restarting"

	event, err := r.classifyEvent(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventDesiredRestarting {
		t.Errorf("terminating pod + desired Restarting classified %q, want %q", event, EventDesiredRestarting)
	}
}

// The wait guard must not depend on another watch event to eventually
// recover a pod stuck Terminating: a dead node produces none. The
// reconcile that defers must itself requeue within the grace window.
func TestReconcile_RecentlyTerminatingPod_SchedulesRequeue(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()
	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning}},
	}
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-terminating-requeue"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("dave", "test-terminating-requeue")
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-terminating-requeue"}
	req := ctrl.Request{NamespacedName: agentKey}
	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(ctx, updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Give the reconciler's own pod a real, FRESH DeletionTimestamp: a
	// finalizer keeps it around after Delete, exactly like a graceful
	// roll mid-flight.
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name: AgentPodName("dave"), Namespace: "test-terminating-requeue"}, pod); err != nil {
		t.Fatalf("getting pod created by bootstrap reconcile: %v", err)
	}
	podPatch := client.MergeFrom(pod.DeepCopy())
	pod.Finalizers = append(pod.Finalizers, "test/block-deletion")
	if err := k8sClient.Patch(ctx, pod, podPatch); err != nil {
		t.Fatalf("adding finalizer: %v", err)
	}
	if err := k8sClient.Delete(ctx, pod); err != nil {
		t.Fatalf("deleting pod: %v", err)
	}
	t.Cleanup(func() {
		fresh := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: AgentPodName("dave"), Namespace: "test-terminating-requeue"}, fresh); err == nil {
			fp := client.MergeFrom(fresh.DeepCopy())
			fresh.Finalizers = nil
			_ = k8sClient.Patch(ctx, fresh, fp)
		}
	})

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %s — the deferral must schedule its own revisit; the dead-node case produces no further watch events", res.RequeueAfter)
	}
	if res.RequeueAfter > terminatingPodGraceWindow+2*time.Second {
		t.Fatalf("RequeueAfter = %s — must revisit within the %s grace window", res.RequeueAfter, terminatingPodGraceWindow)
	}
}
