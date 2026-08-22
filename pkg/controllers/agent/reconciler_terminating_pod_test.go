package agent

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
