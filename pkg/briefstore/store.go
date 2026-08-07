// Package briefstore defines the BriefStore interface and the session brief data types
// used across the Kyber control plane.
//
// The session brief carries continuity information from one agent lifecycle to the next:
// shutdown type, last activity, recent conversation exchanges, and pending messages.
// It is written by the reconciler before pod creation and fetched by the init container
// via the internal HTTP API endpoint /internal/agents/{name}/session-brief.
package briefstore

import (
	"context"
	"errors"
)

// ErrBriefNotFound is returned by Get when no brief exists for the given agent name.
var ErrBriefNotFound = errors.New("brief not found")

// ShutdownType constants describe how a previous session ended.
// These values are written to Brief.ShutdownType and read by the agent at startup.
const (
	// ShutdownTypePlanned indicates a graceful, operator-initiated shutdown.
	ShutdownTypePlanned = "planned"
	// ShutdownTypeUnplanned indicates an unexpected crash or pod failure.
	ShutdownTypeUnplanned = "unplanned"
	// ShutdownTypeWake indicates the agent is starting from a Suspended state due to
	// an incoming message (scale-to-zero wake). Brief.PendingMessages will be populated.
	ShutdownTypeWake = "wake"
	// ShutdownTypePreemption indicates the node was preempted by GCP and the agent
	// was drained gracefully. BriefMetadata.PreemptionContext will be populated.
	ShutdownTypePreemption = "preemption"
)

// RestartReason constants describe why a new agent session is being started.
// These values are written to Brief.RestartReason and read by the agent at startup.
const (
	// RestartReasonFirstBoot indicates this is the agent's first ever session.
	RestartReasonFirstBoot = "first_boot"
	// RestartReasonOperator indicates a deliberate operator-initiated restart.
	RestartReasonOperator = "operator"
	// RestartReasonCrash indicates the previous session ended unexpectedly.
	RestartReasonCrash = "crash"
	// RestartReasonWake indicates the agent is restarting to handle a wake event.
	RestartReasonWake = "wake"
	// RestartReasonPreemption indicates the agent is restarting after a node preemption.
	RestartReasonPreemption = "preemption"
)

// Brief is a session brief for an agent.
// It is constructed by BuildBrief in the agent controller package and stored here.
// The JSON encoding is written to the PV by the init container and read by the agent at startup.
type Brief struct {
	// Version is the schema version. Always 1 for B2.
	Version int `json:"version"`

	// AgentName is the name of the agent this brief is for.
	AgentName string `json:"agent_name"`

	// Timestamp is when this brief was constructed (UTC).
	Timestamp string `json:"timestamp"`

	// ShutdownType describes how the previous session ended.
	// Values: "planned" | "unplanned" | "wake"
	ShutdownType string `json:"shutdown_type"`

	// RestartReason is a free-form explanation for the restart.
	// Examples: "first_boot", "operator", "crash", "wake"
	RestartReason string `json:"restart_reason"`

	// LastActivity is a human-readable description of what the agent was doing
	// before shutdown. May be empty on first boot or after an unplanned crash.
	LastActivity string `json:"last_activity,omitempty"`

	// RecentExchanges is a short tail of the most recent conversation turns.
	// Populated on planned shutdowns; empty on first boot.
	RecentExchanges []Exchange `json:"recent_exchanges,omitempty"`

	// PendingMessages is a list of messages buffered in Redis during suspension.
	// Populated on wake events (B4); always empty in B2.
	PendingMessages []PendingMessage `json:"pending_messages,omitempty"`

	// Metadata holds numeric and string metadata about the previous session.
	Metadata BriefMetadata `json:"metadata"`
}

// Exchange represents a single turn in the agent's conversation history.
type Exchange struct {
	// Role is "user" or "assistant".
	Role string `json:"role"`
	// Content is the message text.
	Content string `json:"content"`
	// Timestamp is when this exchange occurred (RFC3339).
	Timestamp string `json:"timestamp"`
}

// PendingMessage is a message buffered in Redis during agent suspension (populated by B4).
type PendingMessage struct {
	// Source is the originating channel (e.g., "telegram").
	Source string `json:"source"`
	// From is the sender identifier (e.g., a Telegram chat_id).
	From string `json:"from"`
	// Text is the message content.
	Text string `json:"text"`
	// Timestamp is when the message arrived (RFC3339).
	Timestamp string `json:"timestamp"`
}

// PreemptionContext carries GCP preemption details from the node agent to the
// agent controller. It is stored in BriefMetadata so the restarting agent can
// understand why it was interrupted and on which node.
type PreemptionContext struct {
	// InstanceId is the full GCE instance resource name (e.g. projects/p/zones/z/instances/i).
	InstanceId string `json:"instanceId"`
	// Zone is the GCE zone where the preempted node was running (e.g. "us-central1-a").
	Zone string `json:"zone"`
	// Timestamp is when the preemption notice was received (RFC3339).
	Timestamp string `json:"timestamp"`
	// GracefulDrain indicates whether the agent was drained before the node was terminated.
	GracefulDrain bool `json:"gracefulDrain"`
}

// BriefMetadata holds numeric and model metadata from the previous session.
type BriefMetadata struct {
	// PreviousModel is the LLM model used in the previous session.
	PreviousModel string `json:"previous_model,omitempty"`
	// UptimeSeconds is how long the agent ran in the previous session.
	UptimeSeconds int64 `json:"uptime_seconds,omitempty"`
	// RestartCount is the value of status.restartCount at the time of brief construction.
	RestartCount int32 `json:"restart_count,omitempty"`
	// PreemptionContext is populated when ShutdownType is ShutdownTypePreemption.
	// It carries details about the GCE preemption event that triggered this restart.
	PreemptionContext *PreemptionContext `json:"preemptionContext,omitempty"`
}

// BriefStore persists and retrieves session briefs by agent name.
// Implementations must be safe for concurrent access.
type BriefStore interface {
	// Put stores a brief for the given agent, overwriting any prior brief.
	Put(ctx context.Context, agentName string, brief *Brief) error

	// Get returns the most recent brief for the agent, or ErrBriefNotFound if none exists.
	Get(ctx context.Context, agentName string) (*Brief, error)

	// Delete removes the brief for the given agent.
	// Returns nil if the brief did not exist (idempotent).
	Delete(ctx context.Context, agentName string) error
}
