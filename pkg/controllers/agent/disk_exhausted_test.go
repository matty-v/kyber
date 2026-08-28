package agent

import (
	"context"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestClassifyEvent_DiskReserveLifecycle(t *testing.T) {
	r := &AgentReconciler{}
	agent := &kyberv1.Agent{
		Spec: kyberv1.AgentSpec{DesiredPhase: kyberv1.AgentPhaseRunning},
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseRunning,
			Activity: &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{
				DiskReserveReached: true,
			}},
		},
	}

	event, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil || event != EventDiskReserveReached {
		t.Fatalf("reserve reached: event=%q err=%v, want %q", event, err, EventDiskReserveReached)
	}

	agent.Status.Phase = kyberv1.AgentPhaseDiskExhausted
	agent.Status.Activity.Resources.DiskReserveReached = false
	event, err = r.classifyEvent(context.Background(), agent, nil)
	if err != nil || event != EventDiskReserveCleared {
		t.Fatalf("reserve cleared: event=%q err=%v, want %q", event, err, EventDiskReserveCleared)
	}
}
