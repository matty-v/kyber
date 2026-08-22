package agent

import (
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
)

// BriefInput is the set of inputs used to construct a session brief.
// It is a pure-data struct with no k8s or I/O dependencies.
type BriefInput struct {
	// Now is the timestamp to use for Brief.Timestamp.
	// The caller (briefInputForEvent) supplies this via time.Now(); BuildBrief never
	// reads the clock directly, keeping it a pure function.
	Now time.Time

	// ShutdownType describes how the previous session ended.
	// Values: "planned" | "unplanned"
	ShutdownType string

	// RestartReason is a free-form explanation for the restart.
	// Examples: "first_boot", "operator", "crash"
	RestartReason string

	// LastActivity is a human-readable description of what the agent was doing.
	// May be empty on first boot or after a crash where no state was captured.
	LastActivity string

	// RecentExchanges is a short tail of the previous conversation.
	// Populated by planned shutdowns when the agent writes session-state.json.
	// Empty on first boot.
	RecentExchanges []briefstore.Exchange

	// UptimeSeconds is how long the agent ran in the previous session.
	// Derived from status.StartTime at the time of brief construction.
	UptimeSeconds int64

	// PreviousModel is the LLM model that was in use during the previous session.
	// Comes from spec.model or status.currentModel at the time of brief construction.
	PreviousModel string
}

// BuildBrief constructs a session brief from the given Agent CRD and input values.
// It is a pure function — no I/O, no k8s calls, no clock reads — and is safe to call
// from tests. The timestamp comes from input.Now, which briefInputForEvent sets to
// time.Now() so all I/O is isolated in the context-gathering layer.
func BuildBrief(agent *kyberv1.Agent, input BriefInput) *briefstore.Brief {
	return &briefstore.Brief{
		Version:         1,
		AgentName:       agent.Name,
		Timestamp:       input.Now.UTC().Format(time.RFC3339),
		ShutdownType:    input.ShutdownType,
		RestartReason:   input.RestartReason,
		LastActivity:    input.LastActivity,
		RecentExchanges: input.RecentExchanges,
		Metadata: briefstore.BriefMetadata{
			PreviousModel: input.PreviousModel,
			UptimeSeconds: input.UptimeSeconds,
			RestartCount:  agent.Status.RestartCount,
		},
	}
}

// briefInputForEvent constructs the BriefInput for a given state machine event.
// This is a context-gathering function: it calls time.Now() once and records all
// volatile state so that BuildBrief can remain a pure function.
//
// In B2, last activity and recent exchanges are always empty — these will be populated
// in a future task when the controller reads /persist/session-state.json after pod termination.
func briefInputForEvent(agent *kyberv1.Agent, event Event) BriefInput {
	var input BriefInput

	// Capture the current time here so BuildBrief stays pure (no clock reads).
	input.Now = time.Now()

	// Note: EventCRDCreated (first boot) is intentionally not handled here.
	// First boot goes through ActionCreatePVAndPod, which does not write a brief —
	// the init container's `{}` fallback handles first boot per the spec.
	switch event {
	case EventDesiredRunning:
		// Operator restart from Stopped phase.
		input.ShutdownType = "planned"
		input.RestartReason = "operator"

	case EventPodDeleted:
		// Restarting phase: operator triggered a graceful restart.
		input.ShutdownType = "planned"
		input.RestartReason = "operator"

	case EventAutoRestartTriggered:
		// Failed phase: auto-restart after crash.
		input.ShutdownType = "unplanned"
		input.RestartReason = "crash"

	default:
		// Unknown event context — use safe defaults.
		input.ShutdownType = "unplanned"
		input.RestartReason = "unknown"
	}

	// Compute uptime from the agent's start time if available.
	if agent.Status.StartTime != nil {
		input.UptimeSeconds = int64(time.Since(agent.Status.StartTime.Time).Seconds())
		if input.UptimeSeconds < 0 {
			input.UptimeSeconds = 0
		}
	}

	// An explicit spec pin is authoritative. For harness-default agents the
	// runtime-observed status value is the only concrete model we can record.
	input.PreviousModel = agent.Spec.Model
	if input.PreviousModel == "" {
		input.PreviousModel = agent.Status.CurrentModel
	}

	// RecentExchanges are always empty in B2 — populated when session-state.json is read.

	return input
}
