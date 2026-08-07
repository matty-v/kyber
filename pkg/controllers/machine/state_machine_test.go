package machine

import (
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// TestNextPhase_AllTransitions covers all valid transitions in the machine state machine.
func TestNextPhase_AllTransitions(t *testing.T) {
	tests := []struct {
		name       string
		current    kyberv1.MachinePhase
		event      Event
		wantAction Action
		wantNext   kyberv1.MachinePhase
	}{
		// (none) → Provisioning
		{
			name:       "empty phase: CRD created → Provisioning",
			current:    "",
			event:      EventCRDCreated,
			wantAction: ActionCreateInstance,
			wantNext:   kyberv1.MachinePhaseProvisioning,
		},

		// Provisioning transitions
		{
			name:       "Provisioning: instance running, node Ready → Ready",
			current:    kyberv1.MachinePhaseProvisioning,
			event:      EventInstanceRunningNodeReady,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.MachinePhaseReady,
		},
		{
			name:       "Provisioning: instance running, node not Ready → stay Provisioning",
			current:    kyberv1.MachinePhaseProvisioning,
			event:      EventInstanceRunningNodeNotReady,
			wantAction: ActionWaitForNode,
			wantNext:   kyberv1.MachinePhaseProvisioning,
		},
		{
			name:       "Provisioning: instance creation failed → Failed",
			current:    kyberv1.MachinePhaseProvisioning,
			event:      EventInstanceCreationFailed,
			wantAction: ActionMarkFailed,
			wantNext:   kyberv1.MachinePhaseFailed,
		},
		{
			name:       "Provisioning: timeout → Failed",
			current:    kyberv1.MachinePhaseProvisioning,
			event:      EventProvisioningTimeout,
			wantAction: ActionMarkFailed,
			wantNext:   kyberv1.MachinePhaseFailed,
		},

		// Ready transitions
		{
			name:       "Ready: first agent scheduled → Running",
			current:    kyberv1.MachinePhaseReady,
			event:      EventAgentScheduled,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.MachinePhaseRunning,
		},
		{
			name:       "Ready: desired=Stopped → Stopping",
			current:    kyberv1.MachinePhaseReady,
			event:      EventDesiredStopped,
			wantAction: ActionStopVM,
			wantNext:   kyberv1.MachinePhaseStopping,
		},
		{
			name:       "Ready: node disappeared + preempted → Preempted",
			current:    kyberv1.MachinePhaseReady,
			event:      EventNodeDisappearedPreempted,
			wantAction: ActionBeginReplacement,
			wantNext:   kyberv1.MachinePhasePreempted,
		},
		{
			name:       "Ready: node disappeared + not preempted → Failed",
			current:    kyberv1.MachinePhaseReady,
			event:      EventNodeDisappearedFailed,
			wantAction: ActionMarkFailed,
			wantNext:   kyberv1.MachinePhaseFailed,
		},

		// Running transitions
		{
			name:       "Running: desired=Stopped → Stopping",
			current:    kyberv1.MachinePhaseRunning,
			event:      EventDesiredStopped,
			wantAction: ActionStopVM,
			wantNext:   kyberv1.MachinePhaseStopping,
		},
		{
			name:       "Running: all agents removed → Ready",
			current:    kyberv1.MachinePhaseRunning,
			event:      EventAllAgentsRemoved,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.MachinePhaseReady,
		},
		{
			name:       "Running: node disappeared + preempted → Preempted",
			current:    kyberv1.MachinePhaseRunning,
			event:      EventNodeDisappearedPreempted,
			wantAction: ActionBeginReplacement,
			wantNext:   kyberv1.MachinePhasePreempted,
		},
		{
			name:       "Running: node disappeared + not preempted → Failed",
			current:    kyberv1.MachinePhaseRunning,
			event:      EventNodeDisappearedFailed,
			wantAction: ActionMarkFailed,
			wantNext:   kyberv1.MachinePhaseFailed,
		},

		// Stopping transitions
		{
			name:       "Stopping: all agents stopped + VM stopped → Stopped",
			current:    kyberv1.MachinePhaseStopping,
			event:      EventAllAgentsStoppedVMStopped,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.MachinePhaseStopped,
		},
		{
			name:       "Stopping: timeout → force stop → Stopped",
			current:    kyberv1.MachinePhaseStopping,
			event:      EventStoppingTimeout,
			wantAction: ActionForceStopVM,
			wantNext:   kyberv1.MachinePhaseStopped,
		},

		// Stopped transitions
		{
			name:       "Stopped: desired=Running → Provisioning",
			current:    kyberv1.MachinePhaseStopped,
			event:      EventDesiredRunning,
			wantAction: ActionStartInstance,
			wantNext:   kyberv1.MachinePhaseProvisioning,
		},

		// Preempted → Replacing
		{
			name:       "Preempted: replacement started → Replacing",
			current:    kyberv1.MachinePhasePreempted,
			event:      EventReplacementStarted,
			wantAction: ActionCreateReplacementInstance,
			wantNext:   kyberv1.MachinePhaseReplacing,
		},

		// Replacing transitions
		{
			name:       "Replacing: replacement succeeded → Ready",
			current:    kyberv1.MachinePhaseReplacing,
			event:      EventReplacementSucceeded,
			wantAction: ActionCompleteReplacement,
			wantNext:   kyberv1.MachinePhaseReady,
		},
		{
			name:       "Replacing: replacement failed (retries remaining) → Replacing",
			current:    kyberv1.MachinePhaseReplacing,
			event:      EventReplacementFailed,
			wantAction: ActionCreateReplacementInstance,
			wantNext:   kyberv1.MachinePhaseReplacing,
		},
		{
			name:       "Replacing: retry limit reached → Failed",
			current:    kyberv1.MachinePhaseReplacing,
			event:      EventReplacementRetryLimitReached,
			wantAction: ActionMarkReplacementFailed,
			wantNext:   kyberv1.MachinePhaseFailed,
		},

		// Failed transitions
		{
			name:       "Failed: desired=Running (operator re-provision) → Provisioning",
			current:    kyberv1.MachinePhaseFailed,
			event:      EventDesiredRunning,
			wantAction: ActionCreateInstance,
			wantNext:   kyberv1.MachinePhaseProvisioning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NextPhase(tt.current, tt.event)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Action != tt.wantAction {
				t.Errorf("action: got %v, want %v", result.Action, tt.wantAction)
			}
			if result.NextPhase != tt.wantNext {
				t.Errorf("next phase: got %v, want %v", result.NextPhase, tt.wantNext)
			}
		})
	}
}

// TestNextPhase_InvalidTransitions verifies that invalid state transitions return errors.
func TestNextPhase_InvalidTransitions(t *testing.T) {
	invalidCases := []struct {
		name    string
		current kyberv1.MachinePhase
		event   Event
	}{
		{"Deleted cannot transition anywhere", kyberv1.MachinePhaseDeleted, EventDesiredRunning},
		{"Running cannot receive CRD created", kyberv1.MachinePhaseRunning, EventCRDCreated},
		{"Stopped cannot receive agent scheduled", kyberv1.MachinePhaseStopped, EventAgentScheduled},
		{"Provisioning cannot receive desired stopped directly", kyberv1.MachinePhaseProvisioning, EventDesiredStopped},
		{"Ready cannot receive replacement succeeded", kyberv1.MachinePhaseReady, EventReplacementSucceeded},
		{"Replacing cannot receive CRD created", kyberv1.MachinePhaseReplacing, EventCRDCreated},
	}

	for _, tt := range invalidCases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NextPhase(tt.current, tt.event)
			if err == nil {
				t.Errorf("expected error for invalid transition %q + %q, got nil", tt.current, tt.event)
			}
		})
	}
}

// TestShouldReplaceOnPreemption verifies spot vs non-spot preemption behavior.
func TestShouldReplaceOnPreemption(t *testing.T) {
	if !ShouldReplaceOnPreemption(true) {
		t.Error("spot=true should replace on preemption")
	}
	if ShouldReplaceOnPreemption(false) {
		t.Error("spot=false should NOT replace on preemption")
	}
}

// TestReplacementRetryExceeded verifies the retry limit check.
func TestReplacementRetryExceeded(t *testing.T) {
	tests := []struct {
		count   int32
		want    bool
	}{
		{0, false},
		{1, false},
		{2, false},
		{3, true},  // MaxReplacementRetries = 3
		{4, true},
	}
	for _, tt := range tests {
		got := ReplacementRetryExceeded(tt.count)
		if got != tt.want {
			t.Errorf("ReplacementRetryExceeded(%d): got %v, want %v", tt.count, got, tt.want)
		}
	}
}
