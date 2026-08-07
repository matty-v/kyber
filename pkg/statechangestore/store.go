// Package statechangestore accumulates per-agent state-transition counts so
// the State Change Frequency metrics panel is always available without a
// Prometheus TSDB dependency. The accumulator pattern mirrors pkg/tokenstore.
//
// Key schema (Redis):
//
//	accum:state_changes:{namespace}:{agent}  — hash, field = to_state, value = count
package statechangestore

import "context"

// AgentStateCounts holds accumulated state-transition counts for one agent.
type AgentStateCounts struct {
	Agent   string
	ToState string
	Count   int64
}

// Accumulator accumulates per-agent state-transition counts.
// Implementations must be safe for concurrent use.
type Accumulator interface {
	// IncrBy increments the transition count for (namespace, agentName, toState).
	// A zero or negative increment is ignored.
	IncrBy(ctx context.Context, namespace, agentName, toState string, delta int64) error

	// GetAll returns accumulated counts for all (agent, toState) pairs in namespace.
	// Returns an empty slice (not an error) when no data exists.
	GetAll(ctx context.Context, namespace string) ([]AgentStateCounts, error)

	// DeleteAgent removes the accumulated state-change counts for
	// (namespace, agentName). Idempotent — a missing key is a no-op. This hash
	// has no TTL, so the agent finalizer must delete it explicitly (kyber#565).
	DeleteAgent(ctx context.Context, namespace, agentName string) error
}

// ValidState reports whether state is in the allowed set.
//
// The canonical state vocabulary is exported by pkg/tokenreport/activity.go
// (ActivityWorking, ActivityIdle, ActivityUnknown). "unknown" was added to
// the whitelist in kyber#360 Cause F so the snapshot path accepts the same
// states the CRD path's pkg/api/internal_status.go has always accepted.
// "paused" stays as a forward-looking entry for a future pause feature.
// TODO(state-vocab): consolidate these constants into a shared pkg/agentstate
// package so runtime + CP share one source of truth at compile time.
func ValidState(state string) bool {
	switch state {
	case "working", "idle", "paused", "unknown":
		return true
	}
	return false
}
