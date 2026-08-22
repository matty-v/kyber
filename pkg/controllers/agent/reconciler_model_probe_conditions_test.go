package agent

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Pins the probe-message extensions to the ModelUnsupported decision
// table (canary regression 2026-08-22): an inconclusive probe must
// surface as Unknown (never be silently removed), and a rejection must
// carry the CLI's actual output so the operator sees WHY.

func TestRuntimeStatusConditions_ModelUnsupported_UnknownWhenInconclusiveWithMessage(t *testing.T) {
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		ModelSupported:    nil,
		ModelProbeMessage: "Invalid bearer token. Please run /login.",
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported)
	if cond == nil {
		t.Fatal("expected condition present — an inconclusive probe with a diagnostic must not collapse to silence")
	}
	if cond.Status != metav1.ConditionUnknown {
		t.Errorf("status = %v, want Unknown", cond.Status)
	}
	if cond.Reason != "ProbeInconclusive" {
		t.Errorf("reason = %q, want ProbeInconclusive", cond.Reason)
	}
	if !strings.Contains(cond.Message, "Invalid bearer token") {
		t.Errorf("message %q should carry the probe diagnostic", cond.Message)
	}
}

func TestRuntimeStatusConditions_ModelUnsupported_AbsentWhenNeverProbed(t *testing.T) {
	// nil + no message = probe never ran / old image: condition stays
	// absent, matching the pre-existing contract.
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{})
	_ = r.reconcileRuntimeStatusConditions(agent)
	if cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported); cond != nil {
		t.Errorf("expected condition absent when probe never ran, got %+v", cond)
	}
}

func TestRuntimeStatusConditions_ModelUnsupported_TrueCarriesProbeOutput(t *testing.T) {
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		ModelSupported:    boolPtr(false),
		ModelProbeMessage: "There's an issue with the selected model (claude-opus-4-canary-marker).",
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v, want True", cond)
	}
	if !strings.Contains(cond.Message, "claude-opus-4-canary-marker") {
		t.Errorf("message %q should include the probe output naming the rejected model", cond.Message)
	}
}

func TestRuntimeStatusConditions_ModelUnsupported_TrueWithoutMessageStillReadable(t *testing.T) {
	// Legacy reporter path: bool false, no probe output captured.
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{ModelSupported: boolPtr(false)})
	_ = r.reconcileRuntimeStatusConditions(agent)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v, want True", cond)
	}
	if strings.Contains(cond.Message, "Probe output:") {
		t.Errorf("message %q should not reference probe output when none was reported", cond.Message)
	}
}
