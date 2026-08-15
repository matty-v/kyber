package agent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Failed used to leave on the bare spec.desiredPhase==Running, exactly the
// defect kyber#684 fixed for NeedsAuth and MemoryExhausted. That value is
// permanently true for every agent an operator has ever started, and the
// transition it fired runs ActionResetRetryAndCreatePod — which zeroes
// restartCount — so maxRestartRetries was unreachable and a crash-looping agent
// rebuilt its pod on every reconcile, indefinitely.
//
// Measured in production on kyber-falcon (2026-08-14): the v1.0.5 base-image
// bump left two agents unable to start tmux, and each took a brand-new pod
// every ~12s for hours with status.restartCount pinned at 0.
//
// These tests pin both halves of the fix: the standing desiredPhase cannot
// re-fire the edge, and a start-time crash actually counts against the cap.
//
// Fake client, not envtest — the gate is two int64 field reads.

func failedAgent(generation, observed int64, restarts int32) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       rigAgent,
			Namespace:  rigNS,
			Generation: generation,
		},
		Spec: kyberv1.AgentSpec{
			Runtime:      "codex",
			DesiredPhase: kyberv1.AgentPhaseRunning,
		},
		Status: kyberv1.AgentStatus{
			Phase:              kyberv1.AgentPhaseFailed,
			ObservedGeneration: observed,
			RestartCount:       restarts,
		},
	}
}

// The regression itself. A Failed agent whose spec has not changed since its
// last pod must exhaust its retry budget and then HOLD — never hand back
// EventDesiredRunning, which would reset the counter and start the loop over.
func TestFailedGate_HoldsWhenSpecUnchanged(t *testing.T) {
	// Under the cap: auto-restart (which increments), not the resetting edge.
	agent := failedAgent(4, 4, 0)
	r := newGateReconciler(t, agent)

	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if ev != EventAutoRestartTriggered {
		t.Fatalf("a standing desiredPhase=Running must take the counted auto-restart path; got %q want %q",
			ev, EventAutoRestartTriggered)
	}

	// At the cap: the loop must terminate.
	atCap := failedAgent(4, 4, maxRestartRetries)
	r2 := newGateReconciler(t, atCap)

	ev, err = r2.classifyEvent(context.Background(), atCap, nil)
	if err != nil {
		t.Fatalf("classifyEvent at cap: %v", err)
	}
	if ev != EventRetryLimitReached {
		t.Fatalf("an exhausted agent must stay Failed; got %q want %q — this is the hot loop",
			ev, EventRetryLimitReached)
	}
}

// Reconciling an exhausted agent repeatedly must stay quiet. One pass returning
// the right event proves little; the bug was that EVERY pass rebuilt the pod.
func TestFailedGate_ExhaustedAgentStaysQuietAcrossReconciles(t *testing.T) {
	agent := failedAgent(7, 7, maxRestartRetries)
	r := newGateReconciler(t, agent)

	for i := 0; i < 5; i++ {
		ev, err := r.classifyEvent(context.Background(), agent, nil)
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if ev == EventDesiredRunning {
			t.Fatalf("reconcile %d raised EventDesiredRunning — restartCount is reset by its action, so this is the infinite loop", i)
		}
		if ev != EventRetryLimitReached {
			t.Fatalf("reconcile %d: got %q want %q", i, ev, EventRetryLimitReached)
		}
	}
}

// The operator override must still work. /set-resources on a Failed agent
// (kyber#149) patches the spec, so generation runs ahead of the stamped
// observedGeneration — that is a genuinely new instruction and it fires even
// though the retry budget is spent.
func TestFailedGate_SpecChangeFiresEvenAtCap(t *testing.T) {
	agent := failedAgent(9, 8, maxRestartRetries)
	r := newGateReconciler(t, agent)

	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if ev != EventDesiredRunning {
		t.Fatalf("an operator spec change must override the retry cap; got %q want %q", ev, EventDesiredRunning)
	}
}

// An agent that has never had a pod built has nothing to compare against.
// It must take the retry path rather than treating generation=1, observed=0 as
// an operator edit.
func TestFailedGate_NeverStampedTakesRetryPath(t *testing.T) {
	agent := failedAgent(1, 0, 0)
	r := newGateReconciler(t, agent)

	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if ev != EventAutoRestartTriggered {
		t.Fatalf("an unstamped agent must take the counted path; got %q want %q", ev, EventAutoRestartTriggered)
	}
}

// An operator who stopped the agent must not get a surprise pod, spec change or
// not. Mirrors TestRecoveryGate_NeedsAuth_RespectsDesiredPhase.
func TestFailedGate_RespectsDesiredStopped(t *testing.T) {
	agent := failedAgent(9, 8, 0)
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	r := newGateReconciler(t, agent)

	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if ev != EventDesiredStopped {
		t.Fatalf("desiredPhase=Stopped must halt a Failed agent; got %q want %q", ev, EventDesiredStopped)
	}
}

// The second half of the fix: a pod that dies during Starting has to COUNT.
// {Starting, PodDied} shared an action with the schedule-failure row, and that
// action increments nothing — so the cap the tests above rely on could never be
// reached by a crash that happened before the agent ever reached Running, which
// is precisely how the production incident presented (restartCount 0 forever).
func TestFailedGate_StartingPodDiedIncrementsRestartCount(t *testing.T) {
	tr, err := NextPhase(kyberv1.AgentPhaseStarting, EventPodDied)
	if err != nil {
		t.Fatalf("no {Starting, PodDied} transition: %v", err)
	}
	if tr.NextPhase != kyberv1.AgentPhaseFailed {
		t.Fatalf("{Starting, PodDied} must land in Failed; got %q", tr.NextPhase)
	}
	if tr.Action != ActionEmitEventAutoRestart {
		t.Fatalf("{Starting, PodDied} runs %q, which does not increment restartCount — "+
			"a crash during startup would never reach maxRestartRetries; want %q",
			tr.Action, ActionEmitEventAutoRestart)
	}
}
