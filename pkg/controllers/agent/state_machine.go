// Package agent implements the Agent Controller for the Kyber platform.
// This file defines the pure-function state machine for agent lifecycle transitions.
// It has zero k8s dependencies and is fully unit-testable in isolation.
//
// Note: deletion is handled out-of-band in the reconciler via handleDeletion()
// rather than through the state machine, because DeletionTimestamp-driven logic
// does not fit the pure-function model cleanly. The state machine is for
// lifecycle phases of a live agent; deletion is a separate concern.
package agent

import (
	"fmt"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Event represents a trigger that drives a state transition.
type Event string

const (
	// EventCRDCreated fires when a new Agent CRD is observed with no prior phase.
	EventCRDCreated Event = "CRDCreated"
	// EventPodScheduled fires when the pod has been accepted by the scheduler.
	EventPodScheduled Event = "PodScheduled"
	// EventPodScheduleFailed fires when the scheduler cannot place the pod.
	EventPodScheduleFailed Event = "PodScheduleFailed"
	// EventPodReady fires when the pod's readiness probe passes.
	EventPodReady Event = "PodReady"
	// EventStartupTimeout fires when the pod has not become Ready within the startup window (120s).
	EventStartupTimeout Event = "StartupTimeout"
	// EventDesiredStopped fires when an operator sets spec.desiredPhase to
	// Stopped — the authoritative kill switch (#468). Honored from every phase an
	// operator can hit Stop during an incident (Running, Starting, Failed,
	// MemoryExhausted) — see classifyEvent's centralized allowlist,
	// which pre-empts the Failed/MemoryExhausted auto-restart. The Action differs
	// by phase: live/terminal-pod phases tear the pod down
	// (CaptureStateAndDeletePod → Stopping). Running keeps a graceful SIGTERM → Stopping.
	EventDesiredStopped Event = "DesiredStopped"
	// EventDesiredRestarting fires when spec.desiredPhase is set to Restarting while Running.
	EventDesiredRestarting Event = "DesiredRestarting"
	// EventDesiredNeedsAuth fires when an operator sets spec.desiredPhase to
	// NeedsAuth to force a wedged agent down into the re-authorize flow (#395).
	// Honored only from the recoverable phases (Running, Starting, Failed,
	// MemoryExhausted, Stopped) — see classifyEvent. The Action
	// differs by phase: live-pod phases tear the pod down
	// (CaptureStateAndDeletePod), pod-less phases flip status only (UpdateStatus).
	EventDesiredNeedsAuth Event = "DesiredNeedsAuth"
	// EventDesiredRunning fires when spec.desiredPhase is set to Running from Stopped/Failed.
	EventDesiredRunning Event = "DesiredRunning"
	// EventPodDied fires when the pod terminates unexpectedly while the agent is Running.
	EventPodDied Event = "PodDied"
	// EventLivenessFailed fires when k8s kills the pod due to liveness probe failure.
	// TODO(B3): wire this when RestartPolicy changes or a custom liveness monitor is added.
	EventLivenessFailed Event = "LivenessFailed"
	// EventPodTerminated fires when the pod terminates cleanly during Stopping.
	EventPodTerminated Event = "PodTerminated"
	// EventGracePeriodExceeded fires when the pod has not terminated within the grace period (30s).
	EventGracePeriodExceeded Event = "GracePeriodExceeded"
	// EventPodDeleted fires when the pod is fully deleted (used during Restarting).
	EventPodDeleted Event = "PodDeleted"
	// EventAutoRestartTriggered fires when the controller initiates an auto-restart after a crash.
	EventAutoRestartTriggered Event = "AutoRestartTriggered"
	// EventRetryLimitReached fires when restartCount >= 3 and no more retries are allowed.
	EventRetryLimitReached Event = "RetryLimitReached"
	// EventPreemptionNotice fires when the platform receives advance warning that the agent's machine
	// will be preempted, giving the agent time to drain gracefully.
	EventPreemptionNotice Event = "PreemptionNotice"
	// EventMachinePreempted fires when the agent's machine has been preempted (pod is gone or going).
	EventMachinePreempted Event = "MachinePreempted"
	// EventMachineUnavailable fires whenever assigned provider capacity is not
	// Ready. It is provider-neutral and includes both interruption recovery and
	// ordinary infrastructure repair.
	EventMachineUnavailable Event = "MachineUnavailable"
	// EventMachineReady fires when a replacement machine is available and the agent can be rescheduled.
	EventMachineReady Event = "MachineReady"
	// EventOAuthRefreshFailed fires when the runtime's start script exits with
	// its credential-failure code — start-claude.sh code 2 (Anthropic OAuth
	// refresh returned an error) or start-codex.sh code 42 (`codex login
	// status` failed). Requires human re-auth.
	EventOAuthRefreshFailed Event = "OAuthRefreshFailed"
	// EventRuntimeProbeFailed fires when the startup script proves the harness
	// itself cannot execute. It must win over credential failure classification.
	EventRuntimeProbeFailed Event = "RuntimeProbeFailed"
	// EventOOMKilled fires when the agent container was terminated by the
	// kernel OOM killer (kubelet tagged Reason=OOMKilled). Routes to
	// MemoryExhausted phase rather than Failed-with-auto-restart so the
	// operator can bump spec.resources.memory before retrying — auto-
	// restarting on the same too-small limit would crash-loop and hide
	// the underlying problem (kyber#272).
	EventOOMKilled          Event = "OOMKilled"
	EventDiskReserveReached Event = "DiskReserveReached"
	EventDiskReserveCleared Event = "DiskReserveCleared"
)

// Action describes what the reconciler must do as a result of a transition.
type Action string

const (
	// ActionCreatePVAndPod means create the PVC and agent pod (first boot).
	ActionCreatePVAndPod Action = "CreatePVAndPod"
	// ActionWaitForStart means wait for the pod to start (no k8s action required).
	ActionWaitForStart Action = "WaitForStart"
	// ActionLogAndEmitEvent means log the error and emit a k8s event.
	ActionLogAndEmitEvent Action = "LogAndEmitEvent"
	// ActionUpdateStatus means update the CRD status only (no pod action).
	ActionUpdateStatus Action = "UpdateStatus"
	// ActionKillPodAndEmitEvent means delete the pod and emit a k8s event.
	ActionKillPodAndEmitEvent Action = "KillPodAndEmitEvent"
	// ActionSendSIGTERM means gracefully stop the pod (k8s delete with grace period).
	ActionSendSIGTERM Action = "SendSIGTERM"
	// ActionCaptureStateAndDeletePod means delete the pod (state capture is cooperative via PV).
	ActionCaptureStateAndDeletePod Action = "CaptureStateAndDeletePod"
	// ActionEmitEventAutoRestart means emit a crash event; reconciler will requeue to auto-restart.
	ActionEmitEventAutoRestart Action = "EmitEventAutoRestart"
	// ActionKillPodEmitEventAutoRestart means kill the pod, emit event, and trigger auto-restart.
	ActionKillPodEmitEventAutoRestart Action = "KillPodEmitEventAutoRestart"
	// ActionForceKillPod means forcibly delete the pod (no grace period).
	ActionForceKillPod Action = "ForceKillPod"
	// ActionWriteBriefAndCreatePod means write session brief and create a new pod.
	ActionWriteBriefAndCreatePod Action = "WriteBriefAndCreatePod"
	// ActionResetRetryAndCreatePod means reset restartCount and create pod (operator override of Failed).
	ActionResetRetryAndCreatePod Action = "ResetRetryAndCreatePod"
	// ActionStayFailedAndAlert means do not transition; emit alert for operator.
	ActionStayFailedAndAlert Action = "StayFailedAndAlert"
	// ActionDrainAgent means signal the agent to save state and prepare for shutdown due to preemption.
	ActionDrainAgent Action = "DrainAgent"
	// ActionTransitionToWaiting means remove stale pod state and wait for machine capacity.
	ActionTransitionToWaiting Action = "TransitionToWaiting"
)

// TransitionResult is the output of NextPhase: what to do and where to go.
type TransitionResult struct {
	// Action is what the reconciler must execute.
	Action Action
	// NextPhase is the phase to set after executing the action.
	NextPhase kyberv1.AgentPhase
}

// retryResetThreshold is the duration an agent must be Running before restartCount is reset.
const retryResetThreshold = 5 * time.Minute

// retryBackoffBase is the base duration for the exponential backoff.
const retryBackoffBase = 10 * time.Second

// NextPhase is the pure state machine function.
// It takes the current phase and an event, and returns the required action and next phase.
// It has no side effects and no k8s dependencies.
// All k8s operations are performed by the reconciler based on the returned action.
func NextPhase(current kyberv1.AgentPhase, event Event) (TransitionResult, error) {
	type key struct {
		phase kyberv1.AgentPhase
		event Event
	}

	transitions := map[key]TransitionResult{
		// (none) → Creating
		{phase: "", event: EventCRDCreated}: {
			Action:    ActionCreatePVAndPod,
			NextPhase: kyberv1.AgentPhaseCreating,
		},
		// Creating transitions
		{phase: kyberv1.AgentPhaseCreating, event: EventPodScheduled}: {
			Action:    ActionWaitForStart,
			NextPhase: kyberv1.AgentPhaseStarting,
		},
		{phase: kyberv1.AgentPhaseCreating, event: EventPodScheduleFailed}: {
			Action:    ActionLogAndEmitEvent,
			NextPhase: kyberv1.AgentPhaseFailed,
		},
		// Starting transitions
		{phase: kyberv1.AgentPhaseStarting, event: EventPodReady}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseRunning,
		},
		{phase: kyberv1.AgentPhaseStarting, event: EventStartupTimeout}: {
			Action:    ActionKillPodAndEmitEvent,
			NextPhase: kyberv1.AgentPhaseFailed,
		},
		// If the pod disappears or fails to schedule while Starting (e.g. the
		// pod was deleted externally, the node went away, or PVC provisioning
		// failed), transition to Failed so the retry/backoff logic can recreate.
		// Without these transitions the reconciler hits "invalid transition" and
		// the agent is permanently stuck.
		{phase: kyberv1.AgentPhaseStarting, event: EventPodScheduleFailed}: {
			Action:    ActionLogAndEmitEvent,
			NextPhase: kyberv1.AgentPhaseFailed,
		},
		// A pod that dies DURING startup is a crash like any other, so it takes
		// the same action as {Running, PodDied}: it increments restartCount.
		// It used to share ActionLogAndEmitEvent with the row above, which
		// increments nothing — so maxRestartRetries was unreachable for any
		// crash that happened before the agent ever reached Running, and the
		// agent rebuilt its pod forever at the reconcile rate. Observed on
		// kyber-falcon after the v1.0.5 base-image bump: two agents whose tmux
		// could not start took a new pod every ~12s for hours, restartCount
		// pinned at 0 the whole time. It also emitted "PodScheduleFailed /
		// Agent pod failed to schedule" for a pod that had scheduled fine and
		// then crashed, which sent triage at the scheduler instead of the
		// container; ActionEmitEventAutoRestart reports AgentCrashed instead.
		{phase: kyberv1.AgentPhaseStarting, event: EventPodDied}: {
			Action:    ActionEmitEventAutoRestart,
			NextPhase: kyberv1.AgentPhaseFailed,
		},
		// Starting → NeedsAuth (credential failure during startup: exit code 2
		// for Claude Code, 42 for Codex).
		{phase: kyberv1.AgentPhaseStarting, event: EventOAuthRefreshFailed}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseNeedsAuth,
		},
		{phase: kyberv1.AgentPhaseStarting, event: EventRuntimeProbeFailed}: {
			Action: ActionUpdateStatus, NextPhase: kyberv1.AgentPhaseBrokenRuntime,
		},
		// Starting → MemoryExhausted: agent container was OOM-killed during
		// startup. No auto-restart — operator must address the limit (#272).
		{phase: kyberv1.AgentPhaseStarting, event: EventOOMKilled}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseMemoryExhausted,
		},
		// Running transitions
		{phase: kyberv1.AgentPhaseRunning, event: EventDesiredStopped}: {
			Action:    ActionSendSIGTERM,
			NextPhase: kyberv1.AgentPhaseStopping,
		},
		{phase: kyberv1.AgentPhaseRunning, event: EventDesiredRestarting}: {
			Action:    ActionCaptureStateAndDeletePod,
			NextPhase: kyberv1.AgentPhaseRestarting,
		},
		{phase: kyberv1.AgentPhaseRunning, event: EventPodDied}: {
			Action:    ActionEmitEventAutoRestart,
			NextPhase: kyberv1.AgentPhaseFailed,
		},
		// Running → NeedsAuth (credential failure: exit code 2 for Claude Code,
		// 42 for Codex).
		// No auto-restart — human must re-authorize via PWA.
		{phase: kyberv1.AgentPhaseRunning, event: EventOAuthRefreshFailed}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseNeedsAuth,
		},
		{phase: kyberv1.AgentPhaseRunning, event: EventRuntimeProbeFailed}: {
			Action: ActionUpdateStatus, NextPhase: kyberv1.AgentPhaseBrokenRuntime,
		},
		// Running → MemoryExhausted: agent container was OOM-killed mid-
		// session. No auto-restart — bumping spec.resources.memory is the
		// real fix; auto-restart on the same limit would crash-loop and
		// hide it (#272).
		{phase: kyberv1.AgentPhaseRunning, event: EventOOMKilled}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseMemoryExhausted,
		},
		{phase: kyberv1.AgentPhaseRunning, event: EventDiskReserveReached}: {
			Action: ActionUpdateStatus, NextPhase: kyberv1.AgentPhaseDiskExhausted,
		},
		{phase: kyberv1.AgentPhaseDiskExhausted, event: EventDiskReserveCleared}: {
			Action: ActionUpdateStatus, NextPhase: kyberv1.AgentPhaseRunning,
		},
		{phase: kyberv1.AgentPhaseDiskExhausted, event: EventDesiredRunning}: {
			Action: ActionResetRetryAndCreatePod, NextPhase: kyberv1.AgentPhaseStarting,
		},
		{phase: kyberv1.AgentPhaseRunning, event: EventLivenessFailed}: {
			Action:    ActionKillPodEmitEventAutoRestart,
			NextPhase: kyberv1.AgentPhaseFailed,
		},
		// Stopping transitions
		{phase: kyberv1.AgentPhaseStopping, event: EventPodTerminated}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseStopped,
		},
		{phase: kyberv1.AgentPhaseStopping, event: EventGracePeriodExceeded}: {
			Action:    ActionForceKillPod,
			NextPhase: kyberv1.AgentPhaseStopped,
		},
		// Stopped transitions
		{phase: kyberv1.AgentPhaseStopped, event: EventDesiredRunning}: {
			Action:    ActionWriteBriefAndCreatePod,
			NextPhase: kyberv1.AgentPhaseStarting,
		},
		// Restarting transitions
		{phase: kyberv1.AgentPhaseRestarting, event: EventPodDeleted}: {
			Action:    ActionWriteBriefAndCreatePod,
			NextPhase: kyberv1.AgentPhaseStarting,
		},
		// Failed transitions
		{phase: kyberv1.AgentPhaseFailed, event: EventAutoRestartTriggered}: {
			Action:    ActionWriteBriefAndCreatePod,
			NextPhase: kyberv1.AgentPhaseStarting,
		},
		{phase: kyberv1.AgentPhaseFailed, event: EventRetryLimitReached}: {
			Action:    ActionStayFailedAndAlert,
			NextPhase: kyberv1.AgentPhaseFailed,
		},
		{phase: kyberv1.AgentPhaseFailed, event: EventDesiredRunning}: {
			Action:    ActionResetRetryAndCreatePod,
			NextPhase: kyberv1.AgentPhaseStarting,
		},
		// NeedsAuth transitions: operator re-authorizes → restarts the agent.
		//
		// The re-authorization is what this row waits for, and classifyEvent is
		// where that is enforced — it only raises EventDesiredRunning from
		// NeedsAuth when the credential Secret's resourceVersion differs from
		// status.recoveryInput. Before kyber#684 it raised the event on the bare
		// desiredPhase==Running, which is permanently true for every agent, so
		// this row fired on EVERY reconcile and a dead credential rebuilt its pod
		// every ~20s indefinitely. Do not reintroduce an unguarded edge here.
		{phase: kyberv1.AgentPhaseNeedsAuth, event: EventDesiredRunning}: {
			Action:    ActionResetRetryAndCreatePod,
			NextPhase: kyberv1.AgentPhaseStarting,
		},
		// Operator-forced re-auth (#395): drop a wedged agent to NeedsAuth so it
		// can be re-authorized from scratch.
		// Live-pod phases (Running, Starting) must actually delete the pod —
		// CaptureStateAndDeletePod — so a wedged agent stops
		// running on bad state; a bare status flip would leave the stale pod up.
		{phase: kyberv1.AgentPhaseRunning, event: EventDesiredNeedsAuth}: {
			Action:    ActionCaptureStateAndDeletePod,
			NextPhase: kyberv1.AgentPhaseNeedsAuth,
		},
		{phase: kyberv1.AgentPhaseStarting, event: EventDesiredNeedsAuth}: {
			Action:    ActionCaptureStateAndDeletePod,
			NextPhase: kyberv1.AgentPhaseNeedsAuth,
		},
		// Pod-less phases (Failed, MemoryExhausted, Stopped): no pod
		// to delete, so a bare status flip — same Action the OAuth-failure
		// auto-transitions use.
		{phase: kyberv1.AgentPhaseFailed, event: EventDesiredNeedsAuth}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseNeedsAuth,
		},
		{phase: kyberv1.AgentPhaseMemoryExhausted, event: EventDesiredNeedsAuth}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseNeedsAuth,
		},
		{phase: kyberv1.AgentPhaseDiskExhausted, event: EventDesiredNeedsAuth}: {
			Action: ActionCaptureStateAndDeletePod, NextPhase: kyberv1.AgentPhaseNeedsAuth,
		},
		{phase: kyberv1.AgentPhaseStopped, event: EventDesiredNeedsAuth}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.AgentPhaseNeedsAuth,
		},
		// Authoritative Stop kill switch (#468): make desiredPhase=Stopped halt an
		// agent from every phase an operator can hit Stop during an incident, not
		// only Running. Mirrors the #395 re-auth rows above. The Running row
		// (EventDesiredStopped → SIGTERM → Stopping) already exists above and is
		// left unchanged so the healthy-stop path keeps its graceful shutdown.
		// Live/terminal-pod phases (Starting, Failed, MemoryExhausted) route
		// through Stopping via CaptureStateAndDeletePod — a wedged/terminal pod
		// won't honor a graceful SIGTERM, and reusing the pod-termination
		// machinery guarantees the pod is gone before we converge to Stopped.
		// CaptureStateAndDeletePod is idempotent when the pod is already
		// nil/terminal (executeAction guards on pod != nil), so these are safe to
		// run on a crash-looped agent with no live pod.
		{phase: kyberv1.AgentPhaseStarting, event: EventDesiredStopped}: {
			Action:    ActionCaptureStateAndDeletePod,
			NextPhase: kyberv1.AgentPhaseStopping,
		},
		{phase: kyberv1.AgentPhaseFailed, event: EventDesiredStopped}: {
			Action:    ActionCaptureStateAndDeletePod,
			NextPhase: kyberv1.AgentPhaseStopping,
		},
		{phase: kyberv1.AgentPhaseMemoryExhausted, event: EventDesiredStopped}: {
			Action:    ActionCaptureStateAndDeletePod,
			NextPhase: kyberv1.AgentPhaseStopping,
		},
		{phase: kyberv1.AgentPhaseDiskExhausted, event: EventDesiredStopped}: {
			Action: ActionCaptureStateAndDeletePod, NextPhase: kyberv1.AgentPhaseStopping,
		},
		{phase: kyberv1.AgentPhaseBrokenRuntime, event: EventDesiredStopped}: {
			Action: ActionCaptureStateAndDeletePod, NextPhase: kyberv1.AgentPhaseStopping,
		},
		{phase: kyberv1.AgentPhaseWaitingForMachine, event: EventDesiredStopped}: {
			Action:    ActionForceKillPod,
			NextPhase: kyberv1.AgentPhaseStopped,
		},
		// MemoryExhausted transitions: operator bumps memory limit and
		// triggers Restart → recreate the pod with the new limit (#272).
		{phase: kyberv1.AgentPhaseMemoryExhausted, event: EventDesiredRunning}: {
			Action:    ActionResetRetryAndCreatePod,
			NextPhase: kyberv1.AgentPhaseStarting,
		},
		// Preemption: graceful drain path — advance notice received while running
		{phase: kyberv1.AgentPhaseRunning, event: EventPreemptionNotice}: {
			Action:    ActionDrainAgent,
			NextPhase: kyberv1.AgentPhaseDraining,
		},
		// Preemption: drain complete — pod deleted after graceful drain
		{phase: kyberv1.AgentPhaseDraining, event: EventPodDeleted}: {
			Action:    ActionTransitionToWaiting,
			NextPhase: kyberv1.AgentPhaseWaitingForMachine,
		},
		// Preemption: ungraceful — machine died before or without a drain notice
		{phase: kyberv1.AgentPhaseRunning, event: EventMachinePreempted}: {
			Action:    ActionTransitionToWaiting,
			NextPhase: kyberv1.AgentPhaseWaitingForMachine,
		},
		// Preemption: pod was still starting when the machine was preempted
		{phase: kyberv1.AgentPhaseStarting, event: EventMachinePreempted}: {
			Action:    ActionTransitionToWaiting,
			NextPhase: kyberv1.AgentPhaseWaitingForMachine,
		},
		{phase: kyberv1.AgentPhaseCreating, event: EventMachineUnavailable}: {
			Action:    ActionTransitionToWaiting,
			NextPhase: kyberv1.AgentPhaseWaitingForMachine,
		},
		{phase: kyberv1.AgentPhaseStarting, event: EventMachineUnavailable}: {
			Action:    ActionTransitionToWaiting,
			NextPhase: kyberv1.AgentPhaseWaitingForMachine,
		},
		{phase: kyberv1.AgentPhaseRunning, event: EventMachineUnavailable}: {
			Action:    ActionTransitionToWaiting,
			NextPhase: kyberv1.AgentPhaseWaitingForMachine,
		},
		{phase: kyberv1.AgentPhaseRestarting, event: EventMachineUnavailable}: {
			Action:    ActionTransitionToWaiting,
			NextPhase: kyberv1.AgentPhaseWaitingForMachine,
		},
		// Preemption: draining but the node died before the pod finished terminating
		{phase: kyberv1.AgentPhaseDraining, event: EventMachinePreempted}: {
			Action:    ActionTransitionToWaiting,
			NextPhase: kyberv1.AgentPhaseWaitingForMachine,
		},
		// Recovery: replacement machine is ready — write brief and recreate pod
		{phase: kyberv1.AgentPhaseWaitingForMachine, event: EventMachineReady}: {
			Action:    ActionWriteBriefAndCreatePod,
			NextPhase: kyberv1.AgentPhaseStarting,
		},
	}

	k := key{phase: current, event: event}
	result, ok := transitions[k]
	if !ok {
		return TransitionResult{}, fmt.Errorf("invalid transition: phase=%q event=%q", current, event)
	}
	return result, nil
}

// ShouldResetRetryCount returns true if the agent has been in Running phase for at least
// retryResetThreshold (5 minutes), indicating it is stable and the retry counter should be reset.
// stableAt is the time the agent entered the Running phase (e.g., status.StartTime).
func ShouldResetRetryCount(phase kyberv1.AgentPhase, stableAt time.Time) bool {
	if phase != kyberv1.AgentPhaseRunning {
		return false
	}
	return time.Since(stableAt) >= retryResetThreshold
}

// RetryBackoffDuration returns the backoff duration for a given retry attempt (0-indexed).
// The backoff is exponential: 10s, 30s, 90s (multiply by 3 each time).
// retryCount is the value of status.restartCount before this retry.
func RetryBackoffDuration(retryCount int32) time.Duration {
	d := retryBackoffBase
	for i := int32(0); i < retryCount; i++ {
		d *= 3
	}
	return d
}
