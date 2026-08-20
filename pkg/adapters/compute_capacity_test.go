package adapters

import (
	"testing"
	"time"
)

func TestCapacityObservationFromInstance(t *testing.T) {
	createdAt := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		observation InstanceObservation
		wantState   AvailabilityState
		wantReason  AvailabilityReason
	}{
		{
			name:        "pending",
			observation: InstanceObservation{State: InstanceStatePending},
			wantState:   CapacityPending,
			wantReason:  ReasonProvisioning,
		},
		{
			name:        "running",
			observation: InstanceObservation{State: InstanceStateRunning},
			wantState:   CapacityAvailable,
			wantReason:  ReasonReady,
		},
		{
			name:        "stopped",
			observation: InstanceObservation{State: InstanceStateStopped},
			wantState:   CapacityOffline,
			wantReason:  ReasonStopped,
		},
		{
			name:        "failed",
			observation: InstanceObservation{State: InstanceStateFailed},
			wantState:   CapacityFailed,
			wantReason:  ReasonProviderError,
		},
		{
			name:        "unknown",
			observation: InstanceObservation{State: InstanceStateUnknown},
			wantState:   CapacityUnknown,
			wantReason:  ReasonUnknown,
		},
		{
			name: "interruption wins over native state",
			observation: InstanceObservation{
				State:        InstanceStateStopped,
				Interruption: InterruptionPreempted,
			},
			wantState:  CapacityRecovering,
			wantReason: ReasonInterrupted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			observation := tc.observation
			observation.Location = "test-location"
			observation.ExternalIP = "203.0.113.10"
			observation.InternalIP = "10.0.0.10"
			observation.CreatedAt = createdAt
			selector := map[string]string{MachineLabelKey: "worker-1"}

			got := CapacityObservationFromInstance("opaque-ref", selector, observation)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.ProviderRef != "opaque-ref" {
				t.Errorf("ProviderRef = %q, want opaque-ref", got.ProviderRef)
			}
			if got.Location != observation.Location {
				t.Errorf("Location = %q, want %q", got.Location, observation.Location)
			}
			if got.ExternalIP != observation.ExternalIP {
				t.Errorf("ExternalIP = %q, want %q", got.ExternalIP, observation.ExternalIP)
			}
			if got.InternalIP != observation.InternalIP {
				t.Errorf("InternalIP = %q, want %q", got.InternalIP, observation.InternalIP)
			}
			if !got.CreatedAt.Equal(createdAt) {
				t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, createdAt)
			}
			if got.NodeSelector[MachineLabelKey] != "worker-1" {
				t.Errorf("NodeSelector[%q] = %q, want worker-1", MachineLabelKey, got.NodeSelector[MachineLabelKey])
			}

			selector[MachineLabelKey] = "mutated"
			if got.NodeSelector[MachineLabelKey] != "worker-1" {
				t.Error("CapacityObservationFromInstance retained the caller's mutable selector map")
			}
		})
	}
}

func TestCapacityObservationFromInstanceNilSelector(t *testing.T) {
	got := CapacityObservationFromInstance("ref", nil, InstanceObservation{State: InstanceStateRunning})
	if got.NodeSelector != nil {
		t.Errorf("NodeSelector = %#v, want nil", got.NodeSelector)
	}
}
