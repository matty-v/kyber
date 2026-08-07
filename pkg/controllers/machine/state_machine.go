// Package machine implements the Machine Controller for the Kyber platform.
// This file defines the pure-function state machine for machine lifecycle transitions.
// It has zero k8s dependencies and is fully unit-testable in isolation.
//
// Deletion is handled out-of-band in the reconciler via handleDeletion()
// rather than through the state machine, because DeletionTimestamp-driven logic
// does not fit the pure-function model cleanly.
package machine

import (
	"fmt"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Event represents a trigger that drives a machine state transition.
type Event string

const (
	// EventCRDCreated fires when a new Machine CRD is observed with no prior phase.
	EventCRDCreated Event = "CRDCreated"
	// EventInstanceRunningNodeReady fires when the GCE instance is RUNNING and the k8s node is Ready.
	EventInstanceRunningNodeReady Event = "InstanceRunningNodeReady"
	// EventInstanceRunningNodeNotReady fires when the GCE instance is RUNNING but the k8s node is not yet Ready.
	EventInstanceRunningNodeNotReady Event = "InstanceRunningNodeNotReady"
	// EventInstanceCreationFailed fires when the GCE API returns an error creating the instance.
	EventInstanceCreationFailed Event = "InstanceCreationFailed"
	// EventProvisioningTimeout fires when the instance has been Provisioning for more than the timeout.
	EventProvisioningTimeout Event = "ProvisioningTimeout"
	// EventAgentScheduled fires when the first agent pod is scheduled on this machine's node.
	EventAgentScheduled Event = "AgentScheduled"
	// EventAllAgentsRemoved fires when all agent pods have been removed from this machine's node.
	EventAllAgentsRemoved Event = "AllAgentsRemoved"
	// EventDesiredStopped fires when spec.desiredPhase is set to Stopped while Running/Ready.
	EventDesiredStopped Event = "DesiredStopped"
	// EventDesiredRunning fires when spec.desiredPhase is set to Running from Stopped/Failed.
	EventDesiredRunning Event = "DesiredRunning"
	// EventAllAgentsStoppedVMStopped fires when all agents are stopped and the VM has stopped.
	EventAllAgentsStoppedVMStopped Event = "AllAgentsStoppedVMStopped"
	// EventStoppingTimeout fires when the machine has been Stopping for more than the timeout (5 min).
	EventStoppingTimeout Event = "StoppingTimeout"
	// EventNodeDisappearedPreempted fires when the k8s node disappears and GCE confirms preemption.
	EventNodeDisappearedPreempted Event = "NodeDisappearedPreempted"
	// EventNodeDisappearedFailed fires when the k8s node disappears and it is NOT a preemption.
	EventNodeDisappearedFailed Event = "NodeDisappearedFailed"
	// EventReplacementStarted fires when the preemption replacement flow begins.
	EventReplacementStarted Event = "ReplacementStarted"
	// EventReplacementSucceeded fires when the replacement instance is running and the node is Ready.
	EventReplacementSucceeded Event = "ReplacementSucceeded"
	// EventReplacementFailed fires when a replacement attempt fails.
	EventReplacementFailed Event = "ReplacementFailed"
	// EventReplacementRetryLimitReached fires when all replacement retries (3) are exhausted.
	EventReplacementRetryLimitReached Event = "ReplacementRetryLimitReached"
)

// Action describes what the reconciler must do as a result of a transition.
type Action string

const (
	// ActionCreateInstance means call the compute adapter to provision a new VM.
	ActionCreateInstance Action = "CreateInstance"
	// ActionWaitForNode means wait for the k8s node to appear and become Ready (no VM action).
	ActionWaitForNode Action = "WaitForNode"
	// ActionMarkFailed means set phase to Failed and emit a warning event.
	ActionMarkFailed Action = "MarkFailed"
	// ActionUpdateStatus means update the CRD status only (no VM action).
	ActionUpdateStatus Action = "UpdateStatus"
	// ActionStopVM means call the compute adapter to stop the VM.
	ActionStopVM Action = "StopVM"
	// ActionForceStopVM means force-stop the VM (timeout exceeded, agents may still be running).
	ActionForceStopVM Action = "ForceStopVM"
	// ActionStartInstance means call the compute adapter to start the stopped VM.
	ActionStartInstance Action = "StartInstance"
	// ActionBeginReplacement means transition to Preempted state and trigger replacement.
	ActionBeginReplacement Action = "BeginReplacement"
	// ActionCreateReplacementInstance means provision a new VM in the same zone.
	ActionCreateReplacementInstance Action = "CreateReplacementInstance"
	// ActionCompleteReplacement means update status with new instance ID and node name.
	ActionCompleteReplacement Action = "CompleteReplacement"
	// ActionRetryReplacement means retry the replacement (up to 3 attempts).
	ActionRetryReplacement Action = "RetryReplacement"
	// ActionMarkReplacementFailed means emit alert; replacement exhausted all retries.
	ActionMarkReplacementFailed Action = "MarkReplacementFailed"
)

// TransitionResult is the output of NextPhase: what to do and where to go.
type TransitionResult struct {
	// Action is what the reconciler must execute.
	Action Action
	// NextPhase is the phase to set after executing the action.
	NextPhase kyberv1.MachinePhase
}

// Timing constants (match spec's Configuration Surface table).
const (
	// ProvisioningTimeout is how long to wait for Provisioning → Ready before failing.
	ProvisioningTimeout = 10 * time.Minute
	// StoppingTimeout is how long to wait for Stopping → Stopped before force-stopping.
	StoppingTimeout = 5 * time.Minute
	// StaleNodeGrace is how long a NotReady node is tolerated before force-deletion.
	StaleNodeGrace = 5 * time.Minute
	// MaxReplacementRetries is the maximum number of replacement attempts after preemption.
	MaxReplacementRetries = 3
)

// NextPhase is the pure state machine function.
// It takes the current phase and an event, and returns the required action and next phase.
// It has no side effects and no k8s dependencies.
// All compute and k8s operations are performed by the reconciler based on the returned action.
func NextPhase(current kyberv1.MachinePhase, event Event) (TransitionResult, error) {
	type key struct {
		phase kyberv1.MachinePhase
		event Event
	}

	transitions := map[key]TransitionResult{
		// (none) → Provisioning: new Machine CRD, create the VM.
		{phase: "", event: EventCRDCreated}: {
			Action:    ActionCreateInstance,
			NextPhase: kyberv1.MachinePhaseProvisioning,
		},

		// Provisioning transitions.
		{phase: kyberv1.MachinePhaseProvisioning, event: EventInstanceRunningNodeReady}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.MachinePhaseReady,
		},
		{phase: kyberv1.MachinePhaseProvisioning, event: EventInstanceRunningNodeNotReady}: {
			Action:    ActionWaitForNode,
			NextPhase: kyberv1.MachinePhaseProvisioning,
		},
		{phase: kyberv1.MachinePhaseProvisioning, event: EventInstanceCreationFailed}: {
			Action:    ActionMarkFailed,
			NextPhase: kyberv1.MachinePhaseFailed,
		},
		{phase: kyberv1.MachinePhaseProvisioning, event: EventProvisioningTimeout}: {
			Action:    ActionMarkFailed,
			NextPhase: kyberv1.MachinePhaseFailed,
		},

		// Ready transitions.
		{phase: kyberv1.MachinePhaseReady, event: EventAgentScheduled}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.MachinePhaseRunning,
		},
		{phase: kyberv1.MachinePhaseReady, event: EventDesiredStopped}: {
			Action:    ActionStopVM,
			NextPhase: kyberv1.MachinePhaseStopping,
		},
		// Node disappearing from a Ready machine (unexpected) → check preemption.
		{phase: kyberv1.MachinePhaseReady, event: EventNodeDisappearedPreempted}: {
			Action:    ActionBeginReplacement,
			NextPhase: kyberv1.MachinePhasePreempted,
		},
		{phase: kyberv1.MachinePhaseReady, event: EventNodeDisappearedFailed}: {
			Action:    ActionMarkFailed,
			NextPhase: kyberv1.MachinePhaseFailed,
		},

		// Running transitions.
		{phase: kyberv1.MachinePhaseRunning, event: EventDesiredStopped}: {
			Action:    ActionStopVM,
			NextPhase: kyberv1.MachinePhaseStopping,
		},
		{phase: kyberv1.MachinePhaseRunning, event: EventAllAgentsRemoved}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.MachinePhaseReady,
		},
		{phase: kyberv1.MachinePhaseRunning, event: EventNodeDisappearedPreempted}: {
			Action:    ActionBeginReplacement,
			NextPhase: kyberv1.MachinePhasePreempted,
		},
		{phase: kyberv1.MachinePhaseRunning, event: EventNodeDisappearedFailed}: {
			Action:    ActionMarkFailed,
			NextPhase: kyberv1.MachinePhaseFailed,
		},

		// Stopping transitions.
		{phase: kyberv1.MachinePhaseStopping, event: EventAllAgentsStoppedVMStopped}: {
			Action:    ActionUpdateStatus,
			NextPhase: kyberv1.MachinePhaseStopped,
		},
		{phase: kyberv1.MachinePhaseStopping, event: EventStoppingTimeout}: {
			Action:    ActionForceStopVM,
			NextPhase: kyberv1.MachinePhaseStopped,
		},

		// Stopped transitions.
		{phase: kyberv1.MachinePhaseStopped, event: EventDesiredRunning}: {
			Action:    ActionStartInstance,
			NextPhase: kyberv1.MachinePhaseProvisioning,
		},

		// Preempted → Replacing: auto-replacement triggered.
		{phase: kyberv1.MachinePhasePreempted, event: EventReplacementStarted}: {
			Action:    ActionCreateReplacementInstance,
			NextPhase: kyberv1.MachinePhaseReplacing,
		},
		// Preempted → Failed: retry limit reached before create could succeed.
		// This fires when ActionCreateReplacementInstance has failed MaxReplacementRetries
		// times and classifyEvent detects the exhausted count before even attempting another create.
		{phase: kyberv1.MachinePhasePreempted, event: EventReplacementRetryLimitReached}: {
			Action:    ActionMarkReplacementFailed,
			NextPhase: kyberv1.MachinePhaseFailed,
		},

		// Replacing transitions.
		{phase: kyberv1.MachinePhaseReplacing, event: EventReplacementSucceeded}: {
			Action:    ActionCompleteReplacement,
			NextPhase: kyberv1.MachinePhaseReady,
		},
		{phase: kyberv1.MachinePhaseReplacing, event: EventReplacementFailed}: {
			// Replacement VM died (e.g. double preemption) — create a new one.
			// classifyReplacing checks ReplacementRetryExceeded before reaching here.
			Action:    ActionCreateReplacementInstance,
			NextPhase: kyberv1.MachinePhaseReplacing,
		},
		{phase: kyberv1.MachinePhaseReplacing, event: EventReplacementRetryLimitReached}: {
			Action:    ActionMarkReplacementFailed,
			NextPhase: kyberv1.MachinePhaseFailed,
		},

		// Failed transitions: operator can attempt re-provision.
		{phase: kyberv1.MachinePhaseFailed, event: EventDesiredRunning}: {
			Action:    ActionCreateInstance,
			NextPhase: kyberv1.MachinePhaseProvisioning,
		},
	}

	k := key{phase: current, event: event}
	result, ok := transitions[k]
	if !ok {
		return TransitionResult{}, fmt.Errorf("invalid transition: phase=%q event=%q", current, event)
	}
	return result, nil
}

// ShouldReplaceOnPreemption returns true if this is a spot machine that should
// automatically be replaced when preempted.
// Non-spot machines do not auto-replace — they go to Failed.
func ShouldReplaceOnPreemption(spot bool) bool {
	return spot
}

// ReplacementRetryExceeded returns true if the replacement attempt count has
// reached the maximum allowed retries.
func ReplacementRetryExceeded(replacementCount int32) bool {
	return replacementCount >= MaxReplacementRetries
}
