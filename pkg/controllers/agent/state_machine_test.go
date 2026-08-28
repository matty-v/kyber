package agent

import (
	"testing"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// TestNextPhase_AllTransitions covers the transitions in the state machine table.
// RetryLimitReached is covered by TestNextPhase_FailedRetryLimit.
func TestNextPhase_AllTransitions(t *testing.T) {
	tests := []struct {
		name       string
		current    kyberv1.AgentPhase
		event      Event
		wantAction Action
		wantNext   kyberv1.AgentPhase
		wantErr    bool
	}{
		// (none) → Creating
		{
			name:       "empty phase: CRD created → Creating",
			current:    "",
			event:      EventCRDCreated,
			wantAction: ActionCreatePVAndPod,
			wantNext:   kyberv1.AgentPhaseCreating,
		},
		// Creating transitions
		{
			name:       "Creating: pod scheduled → Starting",
			current:    kyberv1.AgentPhaseCreating,
			event:      EventPodScheduled,
			wantAction: ActionWaitForStart,
			wantNext:   kyberv1.AgentPhaseStarting,
		},
		{
			name:       "Creating: pod failed to schedule → Failed",
			current:    kyberv1.AgentPhaseCreating,
			event:      EventPodScheduleFailed,
			wantAction: ActionLogAndEmitEvent,
			wantNext:   kyberv1.AgentPhaseFailed,
		},
		// Starting transitions
		{
			name:       "Starting: liveness probe passes → Running",
			current:    kyberv1.AgentPhaseStarting,
			event:      EventPodReady,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseRunning,
		},
		{
			name:       "Starting: startup timeout → Failed",
			current:    kyberv1.AgentPhaseStarting,
			event:      EventStartupTimeout,
			wantAction: ActionKillPodAndEmitEvent,
			wantNext:   kyberv1.AgentPhaseFailed,
		},
		// Running transitions
		{
			name:       "Running: desired=Stopped → Stopping",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventDesiredStopped,
			wantAction: ActionSendSIGTERM,
			wantNext:   kyberv1.AgentPhaseStopping,
		},
		{
			name:       "Running: desired=Restarting → Restarting",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventDesiredRestarting,
			wantAction: ActionCaptureStateAndDeletePod,
			wantNext:   kyberv1.AgentPhaseRestarting,
		},
		{
			name:       "Running: pod dies (crash), retries available → Failed",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventPodDied,
			wantAction: ActionEmitEventAutoRestart,
			wantNext:   kyberv1.AgentPhaseFailed,
		},
		{
			name:       "Running: liveness probe fails → Failed",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventLivenessFailed,
			wantAction: ActionKillPodEmitEventAutoRestart,
			wantNext:   kyberv1.AgentPhaseFailed,
		},
		// Stopping transitions
		{
			name:       "Stopping: pod terminated → Stopped",
			current:    kyberv1.AgentPhaseStopping,
			event:      EventPodTerminated,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseStopped,
		},
		{
			name:       "Stopping: grace period exceeded → Stopped",
			current:    kyberv1.AgentPhaseStopping,
			event:      EventGracePeriodExceeded,
			wantAction: ActionForceKillPod,
			wantNext:   kyberv1.AgentPhaseStopped,
		},
		// Stopped transitions
		{
			name:       "Stopped: desired=Running → Starting",
			current:    kyberv1.AgentPhaseStopped,
			event:      EventDesiredRunning,
			wantAction: ActionWriteBriefAndCreatePod,
			wantNext:   kyberv1.AgentPhaseStarting,
		},
		// Restarting transitions
		{
			name:       "Restarting: pod deleted → Starting",
			current:    kyberv1.AgentPhaseRestarting,
			event:      EventPodDeleted,
			wantAction: ActionWriteBriefAndCreatePod,
			wantNext:   kyberv1.AgentPhaseStarting,
		},
		// Failed transitions
		{
			name:       "Failed: auto-restart triggered (retries available) → Starting",
			current:    kyberv1.AgentPhaseFailed,
			event:      EventAutoRestartTriggered,
			wantAction: ActionWriteBriefAndCreatePod,
			wantNext:   kyberv1.AgentPhaseStarting,
		},
		{
			name:       "Failed: desired=Running (operator override) → Starting",
			current:    kyberv1.AgentPhaseFailed,
			event:      EventDesiredRunning,
			wantAction: ActionResetRetryAndCreatePod,
			wantNext:   kyberv1.AgentPhaseStarting,
		},
		// NeedsAuth transitions
		{
			name:       "Running + OAuthRefreshFailed → NeedsAuth",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventOAuthRefreshFailed,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseNeedsAuth,
		},
		{
			name:       "Starting + OAuthRefreshFailed → NeedsAuth",
			current:    kyberv1.AgentPhaseStarting,
			event:      EventOAuthRefreshFailed,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseNeedsAuth,
		},
		{
			name:       "Starting + RuntimeProbeFailed → BrokenRuntime",
			current:    kyberv1.AgentPhaseStarting,
			event:      EventRuntimeProbeFailed,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseBrokenRuntime,
		},
		{
			name:       "Running + RuntimeProbeFailed → BrokenRuntime",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventRuntimeProbeFailed,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseBrokenRuntime,
		},
		{
			name:       "BrokenRuntime + DesiredStopped → Stopping",
			current:    kyberv1.AgentPhaseBrokenRuntime,
			event:      EventDesiredStopped,
			wantAction: ActionCaptureStateAndDeletePod,
			wantNext:   kyberv1.AgentPhaseStopping,
		},
		{
			name:       "BrokenRuntime + DesiredRestarting → Restarting",
			current:    kyberv1.AgentPhaseBrokenRuntime,
			event:      EventDesiredRestarting,
			wantAction: ActionCaptureStateAndDeletePod,
			wantNext:   kyberv1.AgentPhaseRestarting,
		},
		{
			name:       "NeedsAuth + DesiredRunning → Starting",
			current:    kyberv1.AgentPhaseNeedsAuth,
			event:      EventDesiredRunning,
			wantAction: ActionResetRetryAndCreatePod,
			wantNext:   kyberv1.AgentPhaseStarting,
		},
		// MemoryExhausted transitions (#272)
		{
			name:       "Running + OOMKilled → MemoryExhausted",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventOOMKilled,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseMemoryExhausted,
		},
		{
			name:       "Starting + OOMKilled → MemoryExhausted",
			current:    kyberv1.AgentPhaseStarting,
			event:      EventOOMKilled,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseMemoryExhausted,
		},
		{
			name:       "MemoryExhausted + DesiredRunning → Starting",
			current:    kyberv1.AgentPhaseMemoryExhausted,
			event:      EventDesiredRunning,
			wantAction: ActionResetRetryAndCreatePod,
			wantNext:   kyberv1.AgentPhaseStarting,
		},
		// DiskExhausted transitions (MAT-10).
		{
			name:       "Running + DiskReserveReached → DiskExhausted",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventDiskReserveReached,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseDiskExhausted,
		},
		{
			name:       "DiskExhausted + DiskReserveCleared → Running",
			current:    kyberv1.AgentPhaseDiskExhausted,
			event:      EventDiskReserveCleared,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseRunning,
		},
		{
			name:       "DiskExhausted + DesiredRunning → Starting",
			current:    kyberv1.AgentPhaseDiskExhausted,
			event:      EventDesiredRunning,
			wantAction: ActionResetRetryAndCreatePod,
			wantNext:   kyberv1.AgentPhaseStarting,
		},
		// Operator-forced re-auth (#395). Live-pod phases delete the pod;
		// pod-less phases flip status only. All land in NeedsAuth.
		{
			name:       "Running + DesiredNeedsAuth → NeedsAuth (deletes pod)",
			current:    kyberv1.AgentPhaseRunning,
			event:      EventDesiredNeedsAuth,
			wantAction: ActionCaptureStateAndDeletePod,
			wantNext:   kyberv1.AgentPhaseNeedsAuth,
		},
		{
			name:       "Starting + DesiredNeedsAuth → NeedsAuth (deletes pod)",
			current:    kyberv1.AgentPhaseStarting,
			event:      EventDesiredNeedsAuth,
			wantAction: ActionCaptureStateAndDeletePod,
			wantNext:   kyberv1.AgentPhaseNeedsAuth,
		},
		{
			name:       "Failed + DesiredNeedsAuth → NeedsAuth (status only)",
			current:    kyberv1.AgentPhaseFailed,
			event:      EventDesiredNeedsAuth,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseNeedsAuth,
		},
		{
			name:       "MemoryExhausted + DesiredNeedsAuth → NeedsAuth (status only)",
			current:    kyberv1.AgentPhaseMemoryExhausted,
			event:      EventDesiredNeedsAuth,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseNeedsAuth,
		},
		{
			name:       "Stopped + DesiredNeedsAuth → NeedsAuth (status only)",
			current:    kyberv1.AgentPhaseStopped,
			event:      EventDesiredNeedsAuth,
			wantAction: ActionUpdateStatus,
			wantNext:   kyberv1.AgentPhaseNeedsAuth,
		},
		// Authoritative Stop kill switch (#468), mirroring the #395 NeedsAuth
		// shape. Live/terminal-pod phases route through Stopping via
		// CaptureStateAndDeletePod (idempotent on a nil/terminal pod) so the pod
		// is provably gone before converging to Stopped. Running keeps its
		// existing graceful SIGTERM row (asserted elsewhere) — not re-added here.
		{
			name:       "Starting + DesiredStopped → Stopping (deletes pod)",
			current:    kyberv1.AgentPhaseStarting,
			event:      EventDesiredStopped,
			wantAction: ActionCaptureStateAndDeletePod,
			wantNext:   kyberv1.AgentPhaseStopping,
		},
		{
			name:       "Failed + DesiredStopped → Stopping (deletes pod, pre-empts auto-restart)",
			current:    kyberv1.AgentPhaseFailed,
			event:      EventDesiredStopped,
			wantAction: ActionCaptureStateAndDeletePod,
			wantNext:   kyberv1.AgentPhaseStopping,
		},
		{
			name:       "MemoryExhausted + DesiredStopped → Stopping (deletes pod)",
			current:    kyberv1.AgentPhaseMemoryExhausted,
			event:      EventDesiredStopped,
			wantAction: ActionCaptureStateAndDeletePod,
			wantNext:   kyberv1.AgentPhaseStopping,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NextPhase(tt.current, tt.event)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
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
		current kyberv1.AgentPhase
		event   Event
	}{
		{"Deleted cannot transition anywhere", kyberv1.AgentPhaseDeleted, EventDesiredRunning},
		{"Running cannot receive CRD created", kyberv1.AgentPhaseRunning, EventCRDCreated},
		{"Stopped cannot receive pod died", kyberv1.AgentPhaseStopped, EventPodDied},
		{"Creating cannot receive desired stopped", kyberv1.AgentPhaseCreating, EventDesiredStopped},
		{"NeedsAuth cannot receive PodDied", kyberv1.AgentPhaseNeedsAuth, EventPodDied},
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

// TestNextPhase_FailedRetryLimit verifies that retry limit transitions stay in Failed.
func TestNextPhase_FailedRetryLimit(t *testing.T) {
	result, err := NextPhase(kyberv1.AgentPhaseFailed, EventRetryLimitReached)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionStayFailedAndAlert {
		t.Errorf("action: got %v, want %v", result.Action, ActionStayFailedAndAlert)
	}
	if result.NextPhase != kyberv1.AgentPhaseFailed {
		t.Errorf("next phase: got %v, want %v (should stay Failed)", result.NextPhase, kyberv1.AgentPhaseFailed)
	}
}

// TestShouldResetRetryCount verifies the 5-minute stable reset logic.
func TestShouldResetRetryCount(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		phase     kyberv1.AgentPhase
		stableAt  time.Time
		wantReset bool
	}{
		{
			name:      "Running for 6 minutes — should reset",
			phase:     kyberv1.AgentPhaseRunning,
			stableAt:  now.Add(-6 * time.Minute),
			wantReset: true,
		},
		{
			name:      "Running for 4 minutes — should NOT reset",
			phase:     kyberv1.AgentPhaseRunning,
			stableAt:  now.Add(-4 * time.Minute),
			wantReset: false,
		},
		{
			name:      "Running for exactly 5 minutes — should reset",
			phase:     kyberv1.AgentPhaseRunning,
			stableAt:  now.Add(-5 * time.Minute),
			wantReset: true,
		},
		{
			name:      "Not Running — should NOT reset",
			phase:     kyberv1.AgentPhaseStopped,
			stableAt:  now.Add(-10 * time.Minute),
			wantReset: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldResetRetryCount(tt.phase, tt.stableAt)
			if got != tt.wantReset {
				t.Errorf("ShouldResetRetryCount(%q, %v ago): got %v, want %v",
					tt.phase, now.Sub(tt.stableAt).Round(time.Second), got, tt.wantReset)
			}
		})
	}
}

// TestPreemptionTransitions verifies the new preemption-related state transitions.
func TestPreemptionTransitions(t *testing.T) {
	tests := []struct {
		name       string
		phase      kyberv1.AgentPhase
		event      Event
		wantPhase  kyberv1.AgentPhase
		wantAction Action
	}{
		{
			name:       "Running + PreemptionNotice → Draining",
			phase:      kyberv1.AgentPhaseRunning,
			event:      EventPreemptionNotice,
			wantPhase:  kyberv1.AgentPhaseDraining,
			wantAction: ActionDrainAgent,
		},
		{
			name:       "Draining + PodDeleted → WaitingForMachine",
			phase:      kyberv1.AgentPhaseDraining,
			event:      EventPodDeleted,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "Running + MachinePreempted → WaitingForMachine",
			phase:      kyberv1.AgentPhaseRunning,
			event:      EventMachinePreempted,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "WaitingForMachine + MachineReady → Starting",
			phase:      kyberv1.AgentPhaseWaitingForMachine,
			event:      EventMachineReady,
			wantPhase:  kyberv1.AgentPhaseStarting,
			wantAction: ActionWriteBriefAndCreatePod,
		},
		{
			name:       "Starting + MachinePreempted → WaitingForMachine",
			phase:      kyberv1.AgentPhaseStarting,
			event:      EventMachinePreempted,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "Creating + MachineUnavailable → WaitingForMachine",
			phase:      kyberv1.AgentPhaseCreating,
			event:      EventMachineUnavailable,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "Starting + MachineUnavailable → WaitingForMachine",
			phase:      kyberv1.AgentPhaseStarting,
			event:      EventMachineUnavailable,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "Running + MachineUnavailable → WaitingForMachine",
			phase:      kyberv1.AgentPhaseRunning,
			event:      EventMachineUnavailable,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "Restarting + MachineUnavailable → WaitingForMachine",
			phase:      kyberv1.AgentPhaseRestarting,
			event:      EventMachineUnavailable,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "WaitingForMachine + DesiredStopped → Stopped",
			phase:      kyberv1.AgentPhaseWaitingForMachine,
			event:      EventDesiredStopped,
			wantPhase:  kyberv1.AgentPhaseStopped,
			wantAction: ActionForceKillPod,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NextPhase(tt.phase, tt.event)
			if err != nil {
				t.Fatalf("no transition for %s + %s: %v", tt.phase, tt.event, err)
			}
			if result.NextPhase != tt.wantPhase {
				t.Errorf("NextPhase: got %q, want %q", result.NextPhase, tt.wantPhase)
			}
			if result.Action != tt.wantAction {
				t.Errorf("Action: got %q, want %q", result.Action, tt.wantAction)
			}
		})
	}
}

// TestRetryBackoffDuration verifies the exponential backoff durations.
func TestRetryBackoffDuration(t *testing.T) {
	tests := []struct {
		retryCount int32
		wantMin    time.Duration
		wantMax    time.Duration
	}{
		{0, 9 * time.Second, 11 * time.Second},  // first retry: ~10s
		{1, 29 * time.Second, 31 * time.Second}, // second retry: ~30s
		{2, 89 * time.Second, 91 * time.Second}, // third retry: ~90s
	}

	for _, tt := range tests {
		t.Run("retry count "+string(rune('0'+tt.retryCount)), func(t *testing.T) {
			got := RetryBackoffDuration(tt.retryCount)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("RetryBackoffDuration(%d): got %v, want between %v and %v",
					tt.retryCount, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}
