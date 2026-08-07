package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/statechangestore"
)

// captureSlog swaps slog.Default() for a buffer-backed logger at WARN level
// and returns the buffer plus a restore func. Used by the fail-soft batch
// validation tests to assert WARN lines fire on dropped entries.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return &buf, func() { slog.SetDefault(prev) }
}

func newSnapshotServer(t *testing.T, ms metricsstore.MetricsStore, sc statechangestore.Accumulator) *httptest.Server {
	t.Helper()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithMetricsStore(ms),
		api.WithStateChangeAccumulator(sc),
	)
	return httptest.NewServer(srv.Handler())
}

func TestHandleStatusSnapshot_WritesActivityPoints(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	body := `{"activity_state_seconds":{"working":10.5,"idle":4.5},"at":"2026-05-25T10:00:00Z"}`
	resp, err := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	// Verify points were stored.
	ctx := context.Background()
	pts, err := ms.RangeQuery(ctx, "ts:activity::han:working", 0, 9999999999)
	if err != nil {
		t.Fatalf("RangeQuery: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("want 1 point for working, got %d", len(pts))
	}
	if pts[0].Value != 10.5 {
		t.Errorf("working point value = %f, want 10.5", pts[0].Value)
	}
}

func TestHandleStatusSnapshot_RecordsStateTransitions(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	body := `{"state_transitions":[{"from":"idle","to":"working","at":"2026-05-25T10:00:00Z"}]}`
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	all, err := sc.GetAll(ctx, "")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 || all[0].ToState != "working" || all[0].Count != 1 {
		t.Errorf("state transitions: got %+v", all)
	}
}

func TestHandleStatusSnapshot_InvalidDNS1123Name(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/internal/agents/INVALID_NAME/status", "application/json",
		strings.NewReader(`{}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestHandleStatusSnapshot_InvalidState_DroppedSoftly verifies the
// kyber#360 Cause F fail-soft contract: a single invalid state name in
// activity_state_seconds is dropped with a WARN log, not rejected as a 400.
// The prior fail-fast behavior killed entire 30+-entry snapshot batches for
// hours when a single runtime emitted an out-of-vocab state — that is the
// silent-data-loss anti-pattern the issue exists to close.
func TestHandleStatusSnapshot_InvalidState_DroppedSoftly(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	body := `{"activity_state_seconds":{"banana":5.0}}`
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (fail-soft)", resp.StatusCode)
	}

	// Invalid entry must NOT be persisted.
	ctx := context.Background()
	pts, _ := ms.RangeQuery(ctx, "ts:activity::han:banana", 0, 9999999999)
	if len(pts) != 0 {
		t.Errorf("invalid state should not be stored; got %d points", len(pts))
	}

	if !strings.Contains(buf.String(), "snapshot: dropping invalid state") {
		t.Errorf("expected WARN log 'snapshot: dropping invalid state'; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "state=banana") {
		t.Errorf("expected WARN log to include state=banana; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "agent=han") {
		t.Errorf("expected WARN log to include agent=han; got %q", buf.String())
	}
}

func TestHandleStatusSnapshot_NegativeDeltaRejected(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	body := `{"activity_state_seconds":{"working":-1.0}}`
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestHandleStatusSnapshot_EmptyPayloadAccepted(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	// Empty payload (fields omitted) must be accepted — rolling-upgrade contract.
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(`{}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
}

// TestHandleStatusSnapshot_DeltaSemantics verifies that consecutive snapshots
// with cumulative totals produce per-interval delta points in the store, not
// raw cumulative values. The PWA sums all points in a window to compute totals;
// cumulative totals would cause massive over-counts after the first interval.
func TestHandleStatusSnapshot_DeltaSemantics(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	// First snapshot: cumulative working=60 since pod start.
	snap1 := `{"activity_state_seconds":{"working":60.0},"at":"2026-05-25T10:00:00Z"}`
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(snap1))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snap1 status: got %d, want 200", resp.StatusCode)
	}

	// Second snapshot: cumulative working=75 (15s elapsed since last snapshot).
	snap2 := `{"activity_state_seconds":{"working":75.0},"at":"2026-05-25T10:00:15Z"}`
	resp, _ = http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(snap2))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snap2 status: got %d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	pts, err := ms.RangeQuery(ctx, "ts:activity::han:working", 0, 9999999999)
	if err != nil {
		t.Fatalf("RangeQuery: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("want 2 delta points, got %d: %+v", len(pts), pts)
	}
	// First delta = 60 - 0 (no prior); second delta = 75 - 60 = 15.
	if pts[0].Value != 60.0 {
		t.Errorf("point[0].Value = %f, want 60.0 (first interval delta)", pts[0].Value)
	}
	if pts[1].Value != 15.0 {
		t.Errorf("point[1].Value = %f, want 15.0 (second interval delta)", pts[1].Value)
	}
	// Sum of deltas equals total working seconds in window (what the PWA computes).
	total := pts[0].Value + pts[1].Value
	if total != 75.0 {
		t.Errorf("sum of deltas = %f, want 75.0", total)
	}
}

// TestHandleStatusSnapshot_InvalidFromTransition_DroppedSoftly verifies
// that a state_transition with an invalid tr.From field is dropped softly
// (200 + WARN log), not rejected with 400. Same kyber#360 Cause F semantics
// as the activity_state_seconds path.
func TestHandleStatusSnapshot_InvalidFromTransition_DroppedSoftly(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	body := `{"state_transitions":[{"from":"banana","to":"working","at":"2026-05-25T10:00:00Z"}]}`
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (fail-soft)", resp.StatusCode)
	}

	// Invalid transition must NOT be counted.
	ctx := context.Background()
	all, _ := sc.GetAll(ctx, "")
	if len(all) != 0 {
		t.Errorf("invalid transition should not be recorded; got %+v", all)
	}

	if !strings.Contains(buf.String(), "snapshot: dropping invalid transition") {
		t.Errorf("expected WARN log 'snapshot: dropping invalid transition'; got %q", buf.String())
	}
}

// TestHandleStatusSnapshot_UnknownStateAccepted is the direct positive
// regression gate for kyber#360 Cause F: 'unknown' is now a valid state
// and is persisted to both MetricsStore and the state-change accumulator.
func TestHandleStatusSnapshot_UnknownStateAccepted(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	body := `{"activity_state_seconds":{"unknown":3.5,"working":12.0},"state_transitions":[{"from":"unknown","to":"working","at":"2026-05-25T10:00:00Z"}]}`
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	pts, err := ms.RangeQuery(ctx, "ts:activity::han:unknown", 0, 9999999999)
	if err != nil {
		t.Fatalf("RangeQuery: %v", err)
	}
	if len(pts) != 1 || pts[0].Value != 3.5 {
		t.Errorf("unknown series: want 1 point of 3.5, got %+v", pts)
	}
	workingPts, _ := ms.RangeQuery(ctx, "ts:activity::han:working", 0, 9999999999)
	if len(workingPts) != 1 || workingPts[0].Value != 12.0 {
		t.Errorf("working series: want 1 point of 12.0, got %+v", workingPts)
	}

	all, _ := sc.GetAll(ctx, "")
	if len(all) != 1 || all[0].ToState != "working" || all[0].Count != 1 {
		t.Errorf("transitions: got %+v, want 1 working", all)
	}
}

// TestHandleStatusSnapshot_MixedValidInvalidBatch_PartialSuccess is the
// flagship fail-soft test: a batch containing one invalid entry and
// several valid entries returns 200 and persists the valid entries. This
// is the failure mode (one stray state name kills the whole batch) that
// silently dropped 30+ entries for hours on the live cluster.
func TestHandleStatusSnapshot_MixedValidInvalidBatch_PartialSuccess(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	body := `{
		"activity_state_seconds": {"working": 30, "idle": 60, "banana": 5},
		"state_transitions": [
			{"from": "idle", "to": "working", "at": "2026-05-25T10:00:00Z"},
			{"from": "working", "to": "banana", "at": "2026-05-25T10:00:30Z"},
			{"from": "banana", "to": "idle", "at": "2026-05-25T10:01:00Z"}
		]
	}`
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	workingPts, _ := ms.RangeQuery(ctx, "ts:activity::han:working", 0, 9999999999)
	idlePts, _ := ms.RangeQuery(ctx, "ts:activity::han:idle", 0, 9999999999)
	bananaPts, _ := ms.RangeQuery(ctx, "ts:activity::han:banana", 0, 9999999999)
	if len(workingPts) != 1 || workingPts[0].Value != 30 {
		t.Errorf("working: want 1 point of 30, got %+v", workingPts)
	}
	if len(idlePts) != 1 || idlePts[0].Value != 60 {
		t.Errorf("idle: want 1 point of 60, got %+v", idlePts)
	}
	if len(bananaPts) != 0 {
		t.Errorf("banana (invalid): should not be stored; got %+v", bananaPts)
	}

	// Two valid transitions: idle→working and (third) banana→idle's `to` is
	// idle (valid). banana→idle has invalid `from` so the entire transition
	// is dropped. working→banana has invalid `to` so it's also dropped.
	all, _ := sc.GetAll(ctx, "")
	if len(all) != 1 {
		t.Errorf("transitions: want 1 (idle→working), got %d: %+v", len(all), all)
	}

	// Two WARN lines expected: one for activity_state_seconds[banana], one or
	// two for the invalid transitions.
	if strings.Count(buf.String(), "snapshot: dropping invalid") < 2 {
		t.Errorf("expected ≥2 WARN drop lines; got %q", buf.String())
	}
}

// TestHandleStatusSnapshot_AllInvalidBatch_Returns200 verifies the edge case:
// every entry in the batch is invalid. The endpoint still returns 200 (the
// payload was structurally well-formed), nothing is stored, and a WARN is
// logged per drop. Returning 200 here is deliberate — the prior 400 made
// the failure invisible until panels stayed dark.
func TestHandleStatusSnapshot_AllInvalidBatch_Returns200(t *testing.T) {
	ms := metricsstore.NewMemoryMetricsStore()
	sc := statechangestore.NewMemoryAccumulator()
	ts := newSnapshotServer(t, ms, sc)
	defer ts.Close()

	body := `{"activity_state_seconds":{"banana":1,"orange":2}}`
	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (well-formed but no valid entries)", resp.StatusCode)
	}
}

func TestHandleStatusSnapshot_NotConfigured(t *testing.T) {
	// Neither MetricsStore nor StateChangeAccumulator wired.
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/internal/agents/han/status", "application/json", strings.NewReader(`{}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
}
