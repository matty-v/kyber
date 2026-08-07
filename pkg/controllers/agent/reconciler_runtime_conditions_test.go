package agent

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// These tests pin the decision table for the two PR-E conditions
// (RuntimeVersionMismatch, ModelUnsupported). The reconcileRuntimeStatusConditions
// function translates Status.Runtime fields into the conditions; the
// PWA renders one badge per condition that's True. See kyber#379.

func boolPtr(b bool) *bool { return &b }

func runtimeAgent(rs kyberv1.AgentRuntimeStatus) *kyberv1.Agent {
	return &kyberv1.Agent{
		Status: kyberv1.AgentStatus{
			Runtime: rs,
		},
	}
}

// --- RuntimeVersionMismatch ----------------------------------------------

func TestRuntimeStatusConditions_VersionMismatch_AbsentWhenNoSignal(t *testing.T) {
	// No installed version yet (pod hasn't reported) → condition absent.
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{})
	_ = r.reconcileRuntimeStatusConditions(agent)
	if cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionRuntimeVersionMismatch); cond != nil {
		t.Errorf("expected condition absent when both versions empty, got %+v", cond)
	}
}

func TestRuntimeStatusConditions_VersionMismatch_AbsentWhenRequestedEmpty(t *testing.T) {
	// Agent boots on baked-in (RequestedVersion=="") → no mismatch
	// possible. This is the common no-override path; we must NOT raise
	// the condition just because installed != empty-string.
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{InstalledVersion: "2.1.119"})
	_ = r.reconcileRuntimeStatusConditions(agent)
	if cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionRuntimeVersionMismatch); cond != nil {
		t.Errorf("expected condition absent when requested empty, got %+v", cond)
	}
}

func TestRuntimeStatusConditions_VersionMismatch_FalseWhenInstalledMatchesRequested(t *testing.T) {
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		InstalledVersion: "2.1.119", RequestedVersion: "2.1.119",
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionRuntimeVersionMismatch)
	if cond == nil {
		t.Fatal("expected condition present")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("status = %v, want False", cond.Status)
	}
	if cond.Reason != "Match" {
		t.Errorf("reason = %q, want Match", cond.Reason)
	}
}

func TestRuntimeStatusConditions_VersionMismatch_TrueWhenDiffers(t *testing.T) {
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		InstalledVersion: "2.0.99", RequestedVersion: "2.1.119",
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionRuntimeVersionMismatch)
	if cond == nil {
		t.Fatal("expected condition present")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("status = %v, want True", cond.Status)
	}
	if cond.Reason != "InstallNotConverged" {
		t.Errorf("reason = %q, want InstallNotConverged", cond.Reason)
	}
	// Message must name both versions so operators see the mismatch
	// without grepping pod logs.
	if cond.Message == "" {
		t.Error("expected non-empty message")
	}
	for _, want := range []string{"2.0.99", "2.1.119"} {
		if !contains(cond.Message, want) {
			t.Errorf("message should name version %q; got %q", want, cond.Message)
		}
	}
}

// --- ModelUnsupported ----------------------------------------------------

func TestRuntimeStatusConditions_ModelUnsupported_AbsentWhenProbeNilDueToOldSidecar(t *testing.T) {
	// ModelSupported is *bool — nil means "report came from a sidecar
	// predating PR-E, OR the probe didn't run." Either way: NO badge.
	// Flipping the badge on absent data would be the silent-failure
	// inversion this PR is supposed to close.
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		InstalledVersion: "2.1.119",
		// ModelSupported left nil.
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	if cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported); cond != nil {
		t.Errorf("expected condition absent when ModelSupported nil, got %+v", cond)
	}
}

func TestRuntimeStatusConditions_ModelUnsupported_FalseWhenProbeOK(t *testing.T) {
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		ModelSupported: boolPtr(true),
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported)
	if cond == nil {
		t.Fatal("expected condition present")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("status = %v, want False", cond.Status)
	}
	if cond.Reason != "ProbeOK" {
		t.Errorf("reason = %q, want ProbeOK", cond.Reason)
	}
}

func TestRuntimeStatusConditions_ModelUnsupported_TrueWhenProbeFailed(t *testing.T) {
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		ModelSupported: boolPtr(false),
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported)
	if cond == nil {
		t.Fatal("expected condition present")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("status = %v, want True", cond.Status)
	}
	if cond.Reason != "ProbeFailed" {
		t.Errorf("reason = %q, want ProbeFailed", cond.Reason)
	}
	// Message must hint the remedy.
	if !contains(cond.Message, "newer Claude Code") {
		t.Errorf("message should hint the remedy 'apply a newer Claude Code version'; got %q", cond.Message)
	}
}

// --- Clear-on-resolve (badge clears within one reconcile cycle) ----------

func TestRuntimeStatusConditions_ClearsWhenResolved(t *testing.T) {
	r := &AgentReconciler{}
	// Start with a True ModelUnsupported (probe failed yesterday).
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		ModelSupported: boolPtr(false),
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	if c := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatal("setup: condition should be True before resolve")
	}
	// New report says probe is OK now.
	agent.Status.Runtime.ModelSupported = boolPtr(true)
	_ = r.reconcileRuntimeStatusConditions(agent)
	c := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionModelUnsupported)
	if c == nil {
		t.Fatal("condition should still be present (False), not removed")
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("condition should flip to False on resolve; got %v", c.Status)
	}
}

func TestRuntimeStatusConditions_VersionMismatchClearsWhenRequestedCleared(t *testing.T) {
	// Agent gets a mismatch True, then operator sets spec.runtimeVersion
	// back to empty (boots on baked-in). RequestedVersion in the next
	// report is "" → condition should be REMOVED (no signal), not
	// linger as stale True.
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		InstalledVersion: "2.0.99", RequestedVersion: "2.1.119",
	})
	_ = r.reconcileRuntimeStatusConditions(agent)
	if c := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionRuntimeVersionMismatch); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatal("setup: condition should be True before resolve")
	}
	agent.Status.Runtime.RequestedVersion = ""
	_ = r.reconcileRuntimeStatusConditions(agent)
	if c := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionRuntimeVersionMismatch); c != nil {
		t.Errorf("condition should be removed when RequestedVersion cleared; got %+v", c)
	}
}

// --- Idempotence ---------------------------------------------------------

func TestRuntimeStatusConditions_IdempotentAcrossReconciles(t *testing.T) {
	// Multiple reconciles against the same inputs should report no
	// change after the first — the patch-when-changed pattern relies
	// on this to avoid churning status updates.
	r := &AgentReconciler{}
	agent := runtimeAgent(kyberv1.AgentRuntimeStatus{
		InstalledVersion: "2.0.99", RequestedVersion: "2.1.119",
		ModelSupported: boolPtr(false),
	})
	if !r.reconcileRuntimeStatusConditions(agent) {
		t.Fatal("first pass should change conditions")
	}
	// LastTransitionTime is set by meta.SetStatusCondition only on
	// status flip — a no-op call should report changed=false. Run it
	// a few times to make sure.
	for i := 0; i < 3; i++ {
		if r.reconcileRuntimeStatusConditions(agent) {
			t.Errorf("pass %d should be a no-op (no change)", i+2)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
