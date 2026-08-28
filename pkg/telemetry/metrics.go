package telemetry

import (
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the OTEL instrumentation scope name for the control plane.
const meterName = "kyber-control-plane"

var (
	meterOnce sync.Once
	initErr   error

	// Agent controller metrics.

	// AgentStateTransitions counts state machine transitions for Agent CRDs.
	// Labels: from (previous phase), to (new phase).
	AgentStateTransitions metric.Int64Counter

	// AgentReconcileLatency records the duration of each Agent reconcile call.
	// Labels: phase (the agent's phase at the start of reconciliation).
	AgentReconcileLatency metric.Float64Histogram

	// AgentReconcileErrors counts reconcile calls that returned an error.
	// Labels: event (the classified event that triggered the reconcile).
	AgentReconcileErrors metric.Int64Counter

	// AgentRestarts counts auto-restart events (pod died and was requeued for restart).
	// Labels: agent (the agent name).
	AgentRestarts metric.Int64Counter

	// AgentTokensTotal counts Claude API tokens consumed per agent.
	// Labels: agent, model, token_type (input|cache_creation|cache_read).
	AgentTokensTotal metric.Int64Counter

	// AgentStateChangesTotal counts transitions into each named state per agent.
	// Labels: agent, to_state.
	AgentStateChangesTotal metric.Int64Counter

	// AgentRequestsTotal counts bounded request/reply lifecycle events.
	// Labels: event, outcome. Agent and request IDs stay out of metrics to avoid
	// unbounded cardinality; they are present in structured audit logs instead.
	AgentRequestsTotal metric.Int64Counter

	// AgentRequestDuration records terminal request latency.
	// Labels: outcome (completed|agent_unavailable|delivery_failed).
	AgentRequestDuration metric.Float64Histogram

	// Machine controller metrics.

	// MachineStateTransitions counts state machine transitions for Machine CRDs.
	// Labels: from (previous phase), to (new phase).
	MachineStateTransitions metric.Int64Counter

	// MachineReconcileLatency records the duration of each Machine reconcile call.
	// Labels: phase (the machine's phase at the start of reconciliation).
	MachineReconcileLatency metric.Float64Histogram

	// MachinePreemptions counts spot VM preemption events detected by the Machine Controller.
	// Labels: machine (the machine name), zone.
	MachinePreemptions metric.Int64Counter
)

// InitMetrics registers all control-plane metric instruments against the global
// OTEL MeterProvider. It is idempotent — subsequent calls do nothing and return
// the same error (if any) from the first call.
//
// Call InitMetrics after telemetry.Init so the global MeterProvider is already set.
// If telemetry is disabled, the global provider is a no-op and all instruments
// silently discard observations.
func InitMetrics() error {
	meterOnce.Do(func() {
		m := otel.Meter(meterName)
		initErr = registerAll(m)
	})
	return initErr
}

// registerAll creates all metric instruments on m. Extracted so tests can call
// it directly with a specific meter without touching the global provider.
func registerAll(m metric.Meter) error {
	var err error

	if AgentStateTransitions, err = m.Int64Counter(
		"kyber_agent_state_transitions_total",
		metric.WithDescription("Total number of Agent CRD state machine transitions. Labels: from, to."),
	); err != nil {
		return fmt.Errorf("registering kyber_agent_state_transitions_total: %w", err)
	}

	if AgentReconcileLatency, err = m.Float64Histogram(
		"kyber_agent_reconcile_duration_seconds",
		metric.WithDescription("Duration of Agent reconcile calls in seconds. Labels: phase."),
		metric.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("registering kyber_agent_reconcile_duration_seconds: %w", err)
	}

	if AgentReconcileErrors, err = m.Int64Counter(
		"kyber_agent_reconcile_errors_total",
		metric.WithDescription("Total number of Agent reconcile calls that returned an error. Labels: event."),
	); err != nil {
		return fmt.Errorf("registering kyber_agent_reconcile_errors_total: %w", err)
	}

	if AgentRestarts, err = m.Int64Counter(
		"kyber_agent_restart_count",
		metric.WithDescription("Total number of Agent auto-restart events. Labels: agent."),
	); err != nil {
		return fmt.Errorf("registering kyber_agent_restart_count: %w", err)
	}

	if AgentTokensTotal, err = m.Int64Counter(
		"kyber_agent_tokens_total",
		metric.WithDescription("Total Claude API tokens consumed per agent. Labels: agent, model, token_type (input|cache_creation|cache_read|output)."),
	); err != nil {
		return fmt.Errorf("registering kyber_agent_tokens_total: %w", err)
	}

	if AgentStateChangesTotal, err = m.Int64Counter(
		"kyber_agent_state_changes_total",
		metric.WithDescription("Total agent CRD transitions into each named state. Labels: agent, to_state."),
	); err != nil {
		return fmt.Errorf("registering kyber_agent_state_changes_total: %w", err)
	}

	if AgentRequestsTotal, err = m.Int64Counter(
		"kyber_agent_requests_total",
		metric.WithDescription("Total bounded agent request/reply lifecycle events. Labels: event, outcome."),
	); err != nil {
		return fmt.Errorf("registering kyber_agent_requests_total: %w", err)
	}

	if AgentRequestDuration, err = m.Float64Histogram(
		"kyber_agent_request_duration_seconds",
		metric.WithDescription("Duration of bounded agent requests reaching a terminal outcome. Labels: outcome."),
		metric.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("registering kyber_agent_request_duration_seconds: %w", err)
	}

	if MachineStateTransitions, err = m.Int64Counter(
		"kyber_machine_state_transitions_total",
		metric.WithDescription("Total number of Machine CRD state machine transitions. Labels: from, to."),
	); err != nil {
		return fmt.Errorf("registering kyber_machine_state_transitions_total: %w", err)
	}

	if MachineReconcileLatency, err = m.Float64Histogram(
		"kyber_machine_reconcile_duration_seconds",
		metric.WithDescription("Duration of Machine reconcile calls in seconds. Labels: phase."),
		metric.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("registering kyber_machine_reconcile_duration_seconds: %w", err)
	}

	if MachinePreemptions, err = m.Int64Counter(
		"kyber_machine_preemptions_total",
		metric.WithDescription("Total number of spot VM preemption events detected. Labels: machine, zone."),
	); err != nil {
		return fmt.Errorf("registering kyber_machine_preemptions_total: %w", err)
	}

	return nil
}
