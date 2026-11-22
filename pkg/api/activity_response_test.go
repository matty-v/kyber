package api

import (
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestAgentActivityPreservesStopIntent(t *testing.T) {
	a := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "stopping"}, Spec: kyberv1.AgentSpec{DesiredPhase: kyberv1.AgentPhaseStopped}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning}}
	got := agentToResponse(a)
	if got.Phase != kyberv1.AgentPhaseRunning || got.DesiredPhase != kyberv1.AgentPhaseStopped {
		t.Fatalf("lifecycle intent lost: %+v", got)
	}
}

func TestAgentLegacyActivityDoesNotInventWorkLease(t *testing.T) {
	a := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning, Activity: &kyberv1.ActivityStatus{State: "working"}}}
	got := agentToResponse(a)
	if got.Activity == nil || got.Activity.State != "working" {
		t.Fatal("legacy diagnostics lost")
	}
	if got.Activity.Work != nil {
		t.Fatal("legacy activity acquired an unverified current-work projection")
	}
}
