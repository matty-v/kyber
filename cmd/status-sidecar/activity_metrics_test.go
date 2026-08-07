package main

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMetrics stands up a Metrics instance backed by a ManualReader so
// tests can collect-and-inspect without an OTLP collector. The reader is
// returned alongside so individual tests can drive Collect.
func newTestMetrics(t *testing.T) (*Metrics, *metric.ManualReader) {
	t.Helper()
	t.Setenv("AGENT_NAME", "alice")
	t.Setenv("KYBER_RUNTIME_TYPE", "claude-code")
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	m, err := newMetrics(provider.Meter("kyber.status-sidecar.test"))
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}
	return m, reader
}

// findCounterPoint returns the first data point on the named counter whose
// attribute set matches every key=value pair in want. Fails the test if
// nothing matches — the assertion shape is "the metric should have emitted
// a point with these labels."
func findCounterPoint[N int64 | float64](
	t *testing.T,
	rm *metricdata.ResourceMetrics,
	metricName string,
	want map[string]string,
) metricdata.DataPoint[N] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != metricName {
				continue
			}
			data, ok := m.Data.(metricdata.Sum[N])
			if !ok {
				t.Fatalf("metric %q is not a Sum[%T]; got %T", metricName, *new(N), m.Data)
			}
			for _, dp := range data.DataPoints {
				if attrsMatch(dp.Attributes, want) {
					return dp
				}
			}
		}
	}
	t.Fatalf("no data point on %q matched %v", metricName, want)
	return metricdata.DataPoint[N]{}
}

func attrsMatch(set attribute.Set, want map[string]string) bool {
	for k, v := range want {
		got, ok := set.Value(attribute.Key(k))
		if !ok || got.AsString() != v {
			return false
		}
	}
	return true
}

func TestBumpActivityCounter_FirstEventDoesNotRecord(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.BumpActivityCounter(ctx, time.Unix(1000, 0), "working")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	// The first call only flips state; no dwell to attribute. Confirm by
	// scanning the activity counter and seeing no points yet.
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != "kyber_agent_activity_state_seconds_total" {
				continue
			}
			sum, ok := mm.Data.(metricdata.Sum[float64])
			if !ok {
				t.Fatalf("expected Sum[float64], got %T", mm.Data)
			}
			if len(sum.DataPoints) != 0 {
				t.Errorf("expected no data points on first event; got %d", len(sum.DataPoints))
			}
		}
	}
}

func TestBumpActivityCounter_TransitionRecordsDwell(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	t0 := time.Unix(1000, 0)
	t1 := time.Unix(1042, 0) // 42s later
	m.BumpActivityCounter(ctx, t0, "working")
	m.BumpActivityCounter(ctx, t1, "idle")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	dp := findCounterPoint[float64](t, &rm,
		"kyber_agent_activity_state_seconds_total",
		map[string]string{"state": "working", "agent": "alice", "runtime": "claude-code"},
	)
	if dp.Value != 42 {
		t.Errorf("dwell: got %v, want 42", dp.Value)
	}
}

func TestBumpActivityCounter_SameStateNoOp(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	t0 := time.Unix(1000, 0)
	m.BumpActivityCounter(ctx, t0, "working")
	m.BumpActivityCounter(ctx, t0.Add(10*time.Second), "working") // same state

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != "kyber_agent_activity_state_seconds_total" {
				continue
			}
			sum, _ := mm.Data.(metricdata.Sum[float64])
			if len(sum.DataPoints) != 0 {
				t.Errorf("same-state call must not emit a counter point; got %d", len(sum.DataPoints))
			}
		}
	}
}

func TestBumpActivityCounter_ClockSkewDropped(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	t0 := time.Unix(1000, 0)
	m.BumpActivityCounter(ctx, t0, "working")
	// Earlier-than-prev timestamp on the next event — could happen if
	// clock skews or events arrive out of order. Drop rather than emit
	// a negative-delta point that the SDK would reject.
	m.BumpActivityCounter(ctx, t0.Add(-5*time.Second), "idle")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != "kyber_agent_activity_state_seconds_total" {
				continue
			}
			sum, _ := mm.Data.(metricdata.Sum[float64])
			if len(sum.DataPoints) != 0 {
				t.Errorf("negative-dwell event must not emit a counter point; got %d", len(sum.DataPoints))
			}
		}
	}
}

func TestRecordEventForwarded_LabelsByType(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.RecordEventForwarded(ctx, "heartbeat")
	m.RecordEventForwarded(ctx, "heartbeat")
	m.RecordEventForwarded(ctx, "activity")
	m.RecordEventForwarded(ctx, "") // empty → "unknown"

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	hb := findCounterPoint[int64](t, &rm,
		"kyber_agent_event_forward_total",
		map[string]string{"type": "heartbeat", "agent": "alice", "runtime": "claude-code"},
	)
	if hb.Value != 2 {
		t.Errorf("heartbeat count: got %d, want 2", hb.Value)
	}
	act := findCounterPoint[int64](t, &rm,
		"kyber_agent_event_forward_total",
		map[string]string{"type": "activity"},
	)
	if act.Value != 1 {
		t.Errorf("activity count: got %d, want 1", act.Value)
	}
	unk := findCounterPoint[int64](t, &rm,
		"kyber_agent_event_forward_total",
		map[string]string{"type": "unknown"},
	)
	if unk.Value != 1 {
		t.Errorf("unknown count: got %d, want 1", unk.Value)
	}
}

func TestRecordEventForwardError_LabelsByReason(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.RecordEventForwardError(ctx, "cp_unreachable")
	m.RecordEventForwardError(ctx, "cp_5xx")
	m.RecordEventForwardError(ctx, "cp_5xx")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	dp := findCounterPoint[int64](t, &rm,
		"kyber_agent_event_forward_errors_total",
		map[string]string{"reason": "cp_5xx"},
	)
	if dp.Value != 2 {
		t.Errorf("cp_5xx count: got %d, want 2", dp.Value)
	}
}

func TestNilMetrics_AllMethodsAreNoOps(t *testing.T) {
	// Defensive nil-receiver behavior. As of kyber#360 Cause E, initOTel no
	// longer returns nil for the empty-endpoint case — it falls back to a
	// noop MeterProvider so the activity-state machine keeps accumulating.
	// The nil-safe methods stay for callers (e.g. test helpers) that still
	// pass nil deliberately; the production main path won't.
	var m *Metrics
	ctx := context.Background()
	m.BumpActivityCounter(ctx, time.Now(), "working")
	m.RecordEventForwarded(ctx, "heartbeat")
	m.RecordEventForwardError(ctx, "cp_5xx")
	m.MarkHeartbeat(time.Now())
	// No panic = pass.
}

func TestClassifyForwardError(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{0, "cp_unreachable"},
		{200, "unknown"}, // non-error path; classifier shouldn't be called for 2xx anyway
		{404, "cp_4xx"},
		{429, "cp_4xx"},
		{500, "cp_5xx"},
		{503, "cp_5xx"},
	}
	for _, tc := range cases {
		if got := classifyForwardError(tc.status); got != tc.want {
			t.Errorf("classifyForwardError(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestMarkHeartbeat_GaugeReportsElapsed(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	// Without a heartbeat the gauge reports -1 (sentinel).
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	dp := findGaugePoint(t, &rm, "kyber_agent_heartbeat_seconds_since_last")
	if dp.Value >= 0 {
		t.Errorf("pre-heartbeat gauge should be the -1 sentinel; got %v", dp.Value)
	}

	// After a heartbeat 2s ago the gauge should be >=2 (real wall-clock).
	m.MarkHeartbeat(time.Now().Add(-2 * time.Second))
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect 2: %v", err)
	}
	dp = findGaugePoint(t, &rm, "kyber_agent_heartbeat_seconds_since_last")
	if dp.Value < 2 || dp.Value > 5 {
		t.Errorf("post-heartbeat gauge: got %v, want roughly 2-5s", dp.Value)
	}
}

func findGaugePoint(t *testing.T, rm *metricdata.ResourceMetrics, name string) metricdata.DataPoint[float64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != name {
				continue
			}
			gauge, ok := mm.Data.(metricdata.Gauge[float64])
			if !ok {
				t.Fatalf("metric %q is not Gauge[float64]; got %T", name, mm.Data)
			}
			if len(gauge.DataPoints) == 0 {
				t.Fatalf("metric %q has no data points", name)
			}
			return gauge.DataPoints[0]
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.DataPoint[float64]{}
}
