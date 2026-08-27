// Per-agent activity / forwarder metrics owned by *Metrics (kyber#256).
//
// Counters and the heartbeat-since-last gauge are populated inline from the
// forwarder + heartbeat loops. The activity-state counter accumulates wall-
// time spent in each state by snapshotting on every state transition: when a
// new event flips the state from "working" to "idle", we add the elapsed
// duration to the prior bucket and reset the timer.
//
// Why a transition-driven counter instead of an observable gauge: the metric
// is "wall-time spent in state X since pod start" which composes into
// classic time-series math (rate(), increase()). A gauge reporting "current
// state" would force operators to integrate themselves.

package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// stateNeverObserved is the sentinel time we treat as "the sidecar has never
// observed an activity event yet." Real timestamps will be far in the future
// of this; the time package zero-value also works but the explicit named
// const makes the intent grep-able.
var stateNeverObserved time.Time

// Metrics owns the four instruments described in #256 plus the small bit of
// state needed to populate them (current activity state + last-heartbeat
// timestamp). Methods are goroutine-safe; the forwarder calls them from the
// HTTP handler's goroutine while the heartbeat loop calls MarkHeartbeat from
// its own goroutine.
type Metrics struct {
	agent   string
	runtime string

	// activityStateSeconds — counter, labels {state, agent, runtime}.
	// Accumulates wall-time spent in each activity state since pod start.
	activityStateSeconds metric.Float64Counter

	// eventForwardTotal — counter, labels {type, agent, runtime}.
	// Bumped on every successful forward of a localhost event to the
	// control plane.
	eventForwardTotal metric.Int64Counter

	// eventForwardErrorsTotal — counter, labels {reason, agent, runtime}.
	// Bumped when a forward fails. `reason` classifies the failure into a
	// small enum so dashboards can split them without alphabet-soup labels.
	eventForwardErrorsTotal metric.Int64Counter
	resourceCPUUsage        metric.Float64Gauge
	resourceCPULimit        metric.Float64Gauge
	resourceMemoryUsed      metric.Int64Gauge
	resourceMemoryLimit     metric.Int64Gauge
	resourceDiskUsed        metric.Int64Gauge
	resourceDiskTotal       metric.Int64Gauge

	// State machine for the activity counter.
	mu             sync.Mutex
	currentState   string    // "" until the first activity event lands
	currentStateAt time.Time // when we entered currentState; stateNeverObserved before first event

	// stateSecs tracks cumulative seconds per state for metrics-snapshot
	// POSTs (kyber#343). Mirrors activityStateSeconds but is readable.
	stateSecs map[string]float64

	// pendingTransitions holds state transitions since the last snapshot POST.
	// Drained on each snapshot tick.
	pendingTransitions []stateChange

	// lastHeartbeatUnixNanos drives the heartbeat-since-last observable
	// gauge. atomic so the gauge callback can read concurrently with the
	// heartbeat loop's writes without a lock.
	lastHeartbeatUnixNanos atomic.Int64
}

type stateChange struct {
	From string
	To   string
	At   time.Time
}

// newMetrics registers all four instruments against the supplied meter.
// Returns an error only on a programming mistake (instrument-name conflict);
// runtime issues — the collector being unreachable, etc. — surface in the
// reader, not here.
func newMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{
		agent:          os.Getenv("AGENT_NAME"),
		runtime:        os.Getenv("KYBER_RUNTIME_TYPE"),
		currentStateAt: stateNeverObserved,
		stateSecs:      map[string]float64{},
	}

	var err error

	if m.activityStateSeconds, err = meter.Float64Counter(
		"kyber_agent_activity_state_seconds_total",
		metric.WithDescription("Cumulative wall-time the agent has spent in each activity state since pod start."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_activity_state_seconds_total: %w", err)
	}

	if m.eventForwardTotal, err = meter.Int64Counter(
		"kyber_agent_event_forward_total",
		metric.WithDescription("Status events successfully forwarded by the sidecar to the control plane."),
		metric.WithUnit("1"),
	); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_event_forward_total: %w", err)
	}

	if m.eventForwardErrorsTotal, err = meter.Int64Counter(
		"kyber_agent_event_forward_errors_total",
		metric.WithDescription("Status-event forward failures, labelled by failure reason."),
		metric.WithUnit("1"),
	); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_event_forward_errors_total: %w", err)
	}
	if m.resourceCPUUsage, err = meter.Float64Gauge("kyber_agent_cpu_usage_cores", metric.WithDescription("Agent pod CPU cores used over the latest sampling interval.")); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_cpu_usage_cores: %w", err)
	}
	if m.resourceCPULimit, err = meter.Float64Gauge("kyber_agent_cpu_limit_cores", metric.WithDescription("Agent pod CPU limit in cores.")); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_cpu_limit_cores: %w", err)
	}
	if m.resourceMemoryUsed, err = meter.Int64Gauge("kyber_agent_memory_used_bytes", metric.WithDescription("Agent pod memory currently in use."), metric.WithUnit("By")); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_memory_used_bytes: %w", err)
	}
	if m.resourceMemoryLimit, err = meter.Int64Gauge("kyber_agent_memory_limit_bytes", metric.WithDescription("Agent pod memory limit."), metric.WithUnit("By")); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_memory_limit_bytes: %w", err)
	}
	if m.resourceDiskUsed, err = meter.Int64Gauge("kyber_agent_disk_used_bytes", metric.WithDescription("Bytes used on the agent persistent volume."), metric.WithUnit("By")); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_disk_used_bytes: %w", err)
	}
	if m.resourceDiskTotal, err = meter.Int64Gauge("kyber_agent_disk_total_bytes", metric.WithDescription("Total bytes on the agent persistent volume."), metric.WithUnit("By")); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_disk_total_bytes: %w", err)
	}

	if _, err = meter.Float64ObservableGauge(
		"kyber_agent_heartbeat_seconds_since_last",
		metric.WithDescription("Seconds since the sidecar's last successful heartbeat post to the control plane. -1 means no heartbeat has been recorded yet."),
		metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			ns := m.lastHeartbeatUnixNanos.Load()
			if ns == 0 {
				o.Observe(-1, m.commonAttrs())
				return nil
			}
			elapsed := time.Since(time.Unix(0, ns)).Seconds()
			o.Observe(elapsed, m.commonAttrs())
			return nil
		}),
	); err != nil {
		return nil, fmt.Errorf("registering kyber_agent_heartbeat_seconds_since_last: %w", err)
	}

	return m, nil
}

func (m *Metrics) RecordResourceUsage(ctx context.Context, usage resourceUsage) {
	if m == nil {
		return
	}
	attrs := m.commonAttrs()
	m.resourceCPUUsage.Record(ctx, float64(usage.CPUUsageMillicores)/1000, attrs)
	if usage.CPULimitMillicores != nil {
		m.resourceCPULimit.Record(ctx, float64(*usage.CPULimitMillicores)/1000, attrs)
	}
	m.resourceMemoryUsed.Record(ctx, usage.MemoryUsedBytes, attrs)
	if usage.MemoryLimitBytes != nil {
		m.resourceMemoryLimit.Record(ctx, *usage.MemoryLimitBytes, attrs)
	}
	m.resourceDiskUsed.Record(ctx, usage.DiskUsedBytes, attrs)
	m.resourceDiskTotal.Record(ctx, usage.DiskTotalBytes, attrs)
}

// commonAttrs returns the {agent, runtime} attribute set every metric carries.
func (m *Metrics) commonAttrs() metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("agent", m.agent),
		attribute.String("runtime", m.runtime),
	)
}

// BumpActivityCounter accumulates wall-time into the prior state's bucket and
// flips the active state to newState as of `at`. Idempotent on repeated calls
// with the same state — that's the no-transition case and we deliberately
// avoid double-counting by treating it as a no-op.
//
// Called from the forwarder when an activity event arrives. Safe to call
// before the first event with any newState; the function records a zero-
// duration entry for the prior (empty) state and starts the timer.
func (m *Metrics) BumpActivityCounter(ctx context.Context, at time.Time, newState string) {
	if m == nil || newState == "" {
		return
	}
	m.mu.Lock()
	prevState := m.currentState
	prevStateAt := m.currentStateAt
	m.currentState = newState
	m.currentStateAt = at
	m.mu.Unlock()

	if prevState == newState {
		// Same state — no transition to record, but advance the timer so
		// the next transition counts only the new dwell. Keeping the
		// previous timestamp would over-count if the caller batched
		// repeated same-state events.
		return
	}
	if prevState == "" || prevStateAt.IsZero() {
		// First observation. There's no prior dwell to attribute.
		return
	}

	dwell := at.Sub(prevStateAt).Seconds()
	if dwell < 0 {
		// Clock skew / out-of-order event. Drop rather than emit a
		// negative counter delta (OTel SDK would reject it anyway).
		return
	}
	m.activityStateSeconds.Add(ctx, dwell, metric.WithAttributes(
		attribute.String("state", prevState),
		attribute.String("agent", m.agent),
		attribute.String("runtime", m.runtime),
	))

	// Also accumulate into the readable stateSecs map and record the
	// transition for the next metrics-snapshot POST (kyber#343).
	m.mu.Lock()
	m.stateSecs[prevState] += dwell
	m.pendingTransitions = append(m.pendingTransitions, stateChange{
		From: prevState,
		To:   newState,
		At:   at,
	})
	m.mu.Unlock()
}

// DrainSnapshot returns a copy of stateSecs and drains pendingTransitions.
// Safe to call concurrently with BumpActivityCounter.
func (m *Metrics) DrainSnapshot() (stateSecs map[string]float64, transitions []stateChange) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Copy stateSecs (monotonically increasing; we send the running total).
	secs := make(map[string]float64, len(m.stateSecs))
	for k, v := range m.stateSecs {
		secs[k] = v
	}
	// Drain pending transitions.
	tr := m.pendingTransitions
	m.pendingTransitions = nil
	return secs, tr
}

// RecordEventForwarded bumps the success counter, labeled by event type.
// eventType comes from the JSON body's `type` field (heartbeat / activity /
// future kinds); blank when the body wasn't parseable JSON, which the caller
// can elect to label "unknown" for grouping.
func (m *Metrics) RecordEventForwarded(ctx context.Context, eventType string) {
	if m == nil {
		return
	}
	if eventType == "" {
		eventType = "unknown"
	}
	m.eventForwardTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("type", eventType),
		attribute.String("agent", m.agent),
		attribute.String("runtime", m.runtime),
	))
}

// RecordEventForwardError bumps the failure counter. `reason` is one of:
//   - "cp_unreachable"  — TCP / DNS error reaching the control plane
//   - "cp_4xx"          — control plane returned a 4xx status
//   - "cp_5xx"          — control plane returned a 5xx status
//   - "bad_request"     — the local POST body itself was unreadable
//
// Callers must classify before calling so dashboards don't drown in
// alphabet-soup labels (free-form error strings).
func (m *Metrics) RecordEventForwardError(ctx context.Context, reason string) {
	if m == nil {
		return
	}
	if reason == "" {
		reason = "unknown"
	}
	m.eventForwardErrorsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason),
		attribute.String("agent", m.agent),
		attribute.String("runtime", m.runtime),
	))
}

// MarkHeartbeat records that a heartbeat post just succeeded. Drives the
// kyber_agent_heartbeat_seconds_since_last gauge.
func (m *Metrics) MarkHeartbeat(at time.Time) {
	if m == nil {
		return
	}
	m.lastHeartbeatUnixNanos.Store(at.UnixNano())
}
