package api

import (
	"encoding/json"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	"testing"
	"time"
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

func TestAdmittedActivityProjectionExpiresAndHidesObserverMetadata(t *testing.T) {
	now := time.Now().UTC()
	a := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "native", UID: "agent-life"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning, Activity: &kyberv1.ActivityStatus{Work: &kyberv1.WorkObservation{AgentUID: "agent-life", PodUID: "private-pod", ContainerID: "private-container", Epoch: "private-epoch", Sequence: 1, Complete: true, Foreground: 1, ObservedAt: metav1.NewMicroTime(now), ReceivedAt: metav1.NewMicroTime(now), LastWorkedAt: metav1.NewMicroTime(now), StateSince: metav1.NewMicroTime(now)}}}}
	got := admittedWorkView(a, now)
	if got == nil || got.State != "working" || got.ForegroundCount != 1 {
		t.Fatalf("live projection=%+v", got)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-") {
		t.Fatal("public projection exposed private observer identity")
	}
	if expired := admittedWorkView(a, now.Add(time.Minute)); expired.State != "unavailable" || expired.ForegroundCount != 0 {
		t.Fatalf("expired projection=%+v", expired)
	}
	a.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	if stopped := admittedWorkView(a, now); stopped.State != "excluded" {
		t.Fatalf("stop intent projection=%+v", stopped)
	}
}
