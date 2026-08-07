package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/statechangestore"
)

// testLogger returns a slog.Logger that discards output at the given level.
// Used by handler tests that don't assert on log output.
func testLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: level}))
}

// TestPostStatusEvent_ForwardsToControlPlane verifies the
// sidecar→control-plane wire shape: same URL pattern, same body.
func TestPostStatusEvent_ForwardsToControlPlane(t *testing.T) {
	var got struct {
		path string
		body string
		auth string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		got.body = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := config{AgentName: "alice", ControlPlaneURL: srv.URL}
	body := []byte(`{"type":"heartbeat","at":"2026-05-03T22:00:00Z"}`)

	if _, err := postToCP(context.Background(), srv.Client(), cfg, "status-event", body); err != nil {
		t.Fatalf("postToCP: %v", err)
	}

	if got.path != "/internal/agents/alice/status-event" {
		t.Errorf("path: got %q, want /internal/agents/alice/status-event", got.path)
	}
	if got.body != string(body) {
		t.Errorf("body forwarded should be verbatim: got %q, want %q", got.body, string(body))
	}
}

// TestPostStatusEvent_SurfacesNon2xxAsError ensures the caller can
// distinguish success from server-side rejection.
func TestPostStatusEvent_SurfacesNon2xxAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	cfg := config{AgentName: "alice", ControlPlaneURL: srv.URL}
	status, err := postToCP(context.Background(), srv.Client(), cfg, "status-event", []byte(`{"type":"heartbeat"}`))
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status; got %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", status)
	}
}

// TestForwarder_RoutesAllPaths verifies kyber#257 Phase C2: the
// localhost forwarder accepts every runtime signal path
// and rewrites each to the corresponding control-plane URL prefix. The
// /event path is also covered by the older end-to-end test below.
func TestForwarder_RoutesAllPaths(t *testing.T) {
	cases := []struct {
		localPath string
		cpPath    string
	}{
		{"/event", "/internal/agents/alice/status-event"},
		{"/token-usage", "/internal/agents/alice/token-usage"},
		{"/runtime-version", "/internal/agents/alice/runtime-version"},
		{"/runtime-catalog", "/internal/agents/alice/runtime-catalog"},
		{"/refresh-token", "/internal/agents/alice/refresh-token"},
		{"/codex-auth", "/internal/agents/alice/codex-auth"},
	}
	for _, tc := range cases {
		t.Run(tc.localPath, func(t *testing.T) {
			var gotPath, gotBody string
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = string(body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer cp.Close()
			cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}
			// Spin up the actual forwarder mux so we exercise the same
			// handlerFunc the production binary uses (no shadow copy).
			mux := http.NewServeMux()
			lg := testLogger(slog.LevelInfo)
			mux.HandleFunc("/event", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, nil, lg, "status-event", true))
			mux.HandleFunc("/token-usage", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, nil, lg, "token-usage", false))
			mux.HandleFunc("/runtime-version", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, nil, lg, "runtime-version", false))
			mux.HandleFunc("/runtime-catalog", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, nil, lg, "runtime-catalog", false))
			mux.HandleFunc("/refresh-token", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, nil, lg, "refresh-token", false))
			mux.HandleFunc("/codex-auth", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, nil, lg, "codex-auth", false))
			side := httptest.NewServer(mux)
			defer side.Close()
			body := []byte(`{"hello":"world"}`)
			resp, err := http.Post(side.URL+tc.localPath, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("forwarder status: got %d, want 204", resp.StatusCode)
			}
			if gotPath != tc.cpPath {
				t.Errorf("cp path: got %q, want %q", gotPath, tc.cpPath)
			}
			if gotBody != string(body) {
				t.Errorf("body verbatim: got %q, want %q", gotBody, string(body))
			}
		})
	}
}

// TestForwarder_ForwardsLocalhostEventToControlPlane runs the
// forwarder against a fake control plane and verifies a
// runtime-binary-style POST (e.g. from kyber-token-reporter) flows
// through end-to-end.
func TestForwarder_ForwardsLocalhostEventToControlPlane(t *testing.T) {
	type recorded struct {
		path string
		body string
	}
	cpEvents := make(chan recorded, 4)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cpEvents <- recorded{path: r.URL.Path, body: string(body)}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()

	// Spin up an in-process forwarder by exercising postStatusEvent
	// the same way runForwarder's HandleFunc does. (runForwarder binds
	// 127.0.0.1:8091 which races with anything else on that port; the
	// in-process approach exercises the same code path without needing
	// a free TCP port.)
	cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}
	mux := http.NewServeMux()
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if _, err := postToCP(r.Context(), &http.Client{Timeout: postTimeout}, cfg, "status-event", body); err != nil {
			http.Error(w, "forward: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	side := httptest.NewServer(mux)
	defer side.Close()

	// Simulate an in-pod runtime binary POSTing an activity event to
	// the sidecar's localhost forwarder.
	body := []byte(`{"type":"activity","state":"working","at":"2026-05-03T22:00:00Z"}`)
	resp, err := http.Post(side.URL+"/event", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post to forwarder: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("forwarder status: got %d, want 204", resp.StatusCode)
	}

	select {
	case rec := <-cpEvents:
		if rec.path != "/internal/agents/alice/status-event" {
			t.Errorf("path: got %q, want /internal/agents/alice/status-event", rec.path)
		}
		if rec.body != string(body) {
			t.Errorf("body forwarded verbatim: got %q, want %q", rec.body, string(body))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("control plane never received the forwarded event")
	}
}

// TestForwardHandler_DebugLogOnReceive_WhenDebugEnabled covers the
// kyber#360 diagnostic-safety-net debug log. Today the forwarder is silent
// on success — "no logs" and "code path never fires" look identical, which
// is exactly what hid Cause C through #356 and #358. When
// KYBER_SIDECAR_LOG_LEVEL=debug, the next "panels stay dark" report is a
// single grep against pod logs.
func TestForwardHandler_DebugLogOnReceive_WhenDebugEnabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}
	mux := http.NewServeMux()
	mux.HandleFunc("/event", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, nil, logger, "status-event", true))
	side := httptest.NewServer(mux)
	defer side.Close()

	resp, err := http.Post(side.URL+"/event", "application/json",
		bytes.NewReader([]byte(`{"type":"activity","state":"working","at":"2026-05-27T13:00:00Z"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if !strings.Contains(buf.String(), "forwarder: received event") {
		t.Errorf("expected debug log line containing 'forwarder: received event'; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "agent=alice") {
		t.Errorf("expected debug log to include agent=alice; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "endpoint=status-event") {
		t.Errorf("expected debug log to include endpoint=status-event; got %q", buf.String())
	}
}

// TestForwardHandler_NoDebugLog_WhenInfoLevel asserts the default (info-
// level) behavior leaves production logs quiet. The debug log line must be
// suppressed when KYBER_SIDECAR_LOG_LEVEL is unset.
func TestForwardHandler_NoDebugLog_WhenInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}
	mux := http.NewServeMux()
	mux.HandleFunc("/event", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, nil, logger, "status-event", true))
	side := httptest.NewServer(mux)
	defer side.Close()

	resp, err := http.Post(side.URL+"/event", "application/json",
		bytes.NewReader([]byte(`{"type":"activity","state":"working","at":"2026-05-27T13:00:00Z"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if strings.Contains(buf.String(), "forwarder: received event") {
		t.Errorf("debug log should be suppressed at info level; got %q", buf.String())
	}
}

// TestPostMetricsSnapshot_DebugLogOnEmpty asserts the empty-skip path is
// observable under debug. The silent early-return at this site is the
// other half of Cause C's invisibility on the live cluster.
func TestPostMetricsSnapshot_DebugLogOnEmpty(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("control plane should not be called on empty snapshot")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cp.Close()
	cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}

	m := &Metrics{stateSecs: map[string]float64{}}
	postMetricsSnapshot(context.Background(), &http.Client{Timeout: postTimeout}, cfg, m, logger)

	if !strings.Contains(buf.String(), "metrics snapshot: empty, skipping") {
		t.Errorf("expected debug log containing 'metrics snapshot: empty, skipping'; got %q", buf.String())
	}
}

// TestPostMetricsSnapshot_DebugLogOnPost asserts the POST path logs counts
// under debug. Combined with the empty-skip line, every snapshot tick is
// observable from a single grep.
func TestPostMetricsSnapshot_DebugLogOnPost(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}

	m := &Metrics{
		stateSecs: map[string]float64{"working": 12.5},
		pendingTransitions: []stateChange{
			{From: "idle", To: "working", At: time.Now()},
		},
	}
	postMetricsSnapshot(context.Background(), cp.Client(), cfg, m, logger)

	if !strings.Contains(buf.String(), "metrics snapshot: posting") {
		t.Errorf("expected debug log containing 'metrics snapshot: posting'; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "stateSecs=1") {
		t.Errorf("expected debug log to include stateSecs=1; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "transitions=1") {
		t.Errorf("expected debug log to include transitions=1; got %q", buf.String())
	}
}

// TestInitOTel_EmptyEndpoint_StillTracksActivity is the regression gate for
// kyber#360 Cause E. Before the fix, initOTel returned a nil *Metrics when
// KYBER_OTEL_ENDPOINT was empty (the production default after #358 disabled
// the OTel exporter). Every *Metrics method then short-circuited on the nil
// receiver, including BumpActivityCounter — so the activity-state machine
// silently no-opped for two release cycles while the heartbeat path looked
// healthy. Decoupling state-tracking from export means an empty endpoint
// still constructs a usable *Metrics (backed by a noop MeterProvider) and
// the state machine keeps accumulating.
func TestInitOTel_EmptyEndpoint_StillTracksActivity(t *testing.T) {
	t.Setenv("AGENT_NAME", "alice")
	t.Setenv("KYBER_RUNTIME_TYPE", "claude-code")

	ctx := context.Background()
	m, shutdown, err := initOTel(ctx, "")
	if err != nil {
		t.Fatalf("initOTel(ctx, \"\"): %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	if m == nil {
		t.Fatal("initOTel returned nil *Metrics for empty endpoint; activity state machine is disabled")
	}

	// Drive two transitions so BumpActivityCounter accumulates into stateSecs
	// + records a pending transition. The first event seeds currentState; the
	// second produces a dwell to attribute to the prior bucket.
	start := time.Now()
	m.BumpActivityCounter(ctx, start, "working")
	m.BumpActivityCounter(ctx, start.Add(3*time.Second), "idle")

	stateSecs, transitions := m.DrainSnapshot()
	if got := stateSecs["working"]; got < 2.5 || got > 3.5 {
		t.Errorf("stateSecs[working]: got %v, want ~3.0", got)
	}
	if len(transitions) != 1 {
		t.Fatalf("transitions: got %d, want 1", len(transitions))
	}
	if transitions[0].From != "working" || transitions[0].To != "idle" {
		t.Errorf("transition: got %s→%s, want working→idle", transitions[0].From, transitions[0].To)
	}
}

// newRealCP builds a test CP that runs the actual handleStatusSnapshot
// against in-memory MetricsStore + StateChangeAccumulator. The /status-event
// path drops into a 204 stub because that CP route patches a CRD via a k8s
// client we don't wire here — kyber#360 Cause F is about the snapshot path,
// not the CRD path. statusEvents is signalled once per status-event forward
// so callers can synchronize on the proxy hop the way the prior fake did.
//
// Regression context: the prior test fake at this spot returned 204 to
// every POST without ever invoking validation. That's the gap that let
// kyber#360 Cause F ship — a payload containing 'unknown' was rejected by
// the production CP with a 400, but the sidecar test's fake gave the
// snapshot path a green light. Routing this test through the real handler
// makes the next vocab drift fail in CI, not in production.
func newRealCP(t *testing.T, statusEvents chan struct{}) (*httptest.Server, metricsstore.MetricsStore, statechangestore.Accumulator) {
	t.Helper()
	metricsStore := metricsstore.NewMemoryMetricsStore()
	stateChangeAcc := statechangestore.NewMemoryAccumulator()
	internalSrv := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithMetricsStore(metricsStore),
		api.WithStateChangeAccumulator(stateChangeAcc),
	)
	realHandler := internalSrv.Handler()
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/agents/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status-event") {
			// CRD patch path: needs a k8s client we don't wire here. Stub 204
			// and signal the channel so callers can synchronize on the hop.
			_, _ = io.Copy(io.Discard, r.Body)
			if statusEvents != nil {
				statusEvents <- struct{}{}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// /status (snapshot) and any other internal route exercises the real
		// handleStatusSnapshot — the assertion this test exists to make.
		realHandler.ServeHTTP(w, r)
	})
	return httptest.NewServer(mux), metricsStore, stateChangeAcc
}

// TestSidecarPipeline_OtelDisabled_PostsNonEmptySnapshot drives the
// forwardHandler → BumpActivityCounter → postMetricsSnapshot chain end-to-
// end against the REAL CP handler. The prior version of this test ran the
// snapshot POST against a no-op fake CP that returned 204 unconditionally —
// kyber#360 Cause F (the CP 400'd on 'unknown' states) sailed past it
// because the fake never invoked validation. Now the snapshot lands in an
// in-memory MetricsStore via the production handleStatusSnapshot, so any
// drift on either side of the wire trips this test in CI.
//
// The integration test in test/integration/metrics_pipeline_test.go covers
// the bottom half (CP handler → MetricsStore → API read) with byte-for-byte
// snapshot bodies; this test covers the top half (sidecar event ingest →
// snapshot POST) so the full chain is gated by CI.
func TestSidecarPipeline_OtelDisabled_PostsNonEmptySnapshot(t *testing.T) {
	t.Setenv("AGENT_NAME", "alice")
	t.Setenv("KYBER_RUNTIME_TYPE", "claude-code")

	statusEvents := make(chan struct{}, 4)
	cp, metricsStore, stateChangeAcc := newRealCP(t, statusEvents)
	defer cp.Close()

	ctx := context.Background()
	m, shutdown, err := initOTel(ctx, "")
	if err != nil {
		t.Fatalf("initOTel: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}
	logger := testLogger(slog.LevelInfo)
	mux := http.NewServeMux()
	mux.HandleFunc("/event", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, m, logger, "status-event", true))
	side := httptest.NewServer(mux)
	defer side.Close()

	// Two activity events separated in wall-clock so the second produces a
	// non-zero dwell for the prior state. Runtime binaries (kyber-token-
	// reporter, etc.) emit the same wire shape on every transition.
	for _, ev := range []string{
		`{"type":"activity","state":"working","at":"2026-05-27T18:00:00Z"}`,
		`{"type":"activity","state":"idle","at":"2026-05-27T18:00:00Z"}`,
	} {
		// Sleep between POSTs so BumpActivityCounter sees a non-zero dwell
		// (it derives dwell from time.Now at receive, not the body's `at`).
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Post(side.URL+"/event", "application/json", bytes.NewReader([]byte(ev)))
		if err != nil {
			t.Fatalf("post /event: %v", err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("forwarder status: got %d, want 204", resp.StatusCode)
		}
		_ = resp.Body.Close()
		select {
		case <-statusEvents:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("forward POST never reached CP")
		}
	}

	// Drive a snapshot tick. With Cause E unfixed, *Metrics is nil and this
	// returns silently without ever POSTing — the failure mode this test
	// exists to catch. With Cause F unfixed, the snapshot would be 400'd by
	// the real CP — also caught here.
	postMetricsSnapshot(ctx, cp.Client(), cfg, m, testLogger(slog.LevelInfo))

	// Assert the data ACTUALLY LANDED in the store, not just that the CP
	// returned 2xx. The prior fake CP made this assertion impossible.
	workingPts, err := metricsStore.RangeQuery(ctx, "ts:activity::alice:working", 0, 9999999999)
	if err != nil {
		t.Fatalf("RangeQuery working: %v", err)
	}
	if len(workingPts) == 0 {
		t.Fatal("no 'working' activity points stored — snapshot did not reach the real handler (Cause E/F regression)")
	}
	if workingPts[0].Value <= 0 {
		t.Errorf("activity point value: got %v, want >0", workingPts[0].Value)
	}

	all, err := stateChangeAcc.GetAll(ctx, "")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	sawIdleTransition := false
	for _, c := range all {
		if c.ToState == "idle" && c.Count == 1 {
			sawIdleTransition = true
		}
	}
	if !sawIdleTransition {
		t.Errorf("expected idle transition in accumulator; got %+v", all)
	}
}

// TestSidecarPipeline_UnknownStateAcceptedByRealCP is the direct regression
// gate for kyber#360 Cause F. The runtime's ActivityDetector emits
// 'unknown' on detector errors (pkg/tokenreport/activity.go's
// ActivityUnknown constant); before this fix the CP returned 400 on the
// whole snapshot batch and the runtime's signal was silently lost. This
// test drives the sidecar→real-CP chain with one 'unknown' transition
// followed by one 'working' transition and asserts both are persisted to
// the MetricsStore.
func TestSidecarPipeline_UnknownStateAcceptedByRealCP(t *testing.T) {
	t.Setenv("AGENT_NAME", "alice")
	t.Setenv("KYBER_RUNTIME_TYPE", "claude-code")

	statusEvents := make(chan struct{}, 4)
	cp, metricsStore, stateChangeAcc := newRealCP(t, statusEvents)
	defer cp.Close()

	ctx := context.Background()
	m, shutdown, err := initOTel(ctx, "")
	if err != nil {
		t.Fatalf("initOTel: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}
	logger := testLogger(slog.LevelInfo)
	mux := http.NewServeMux()
	mux.HandleFunc("/event", forwardHandler(&http.Client{Timeout: postTimeout}, cfg, m, logger, "status-event", true))
	side := httptest.NewServer(mux)
	defer side.Close()

	for _, ev := range []string{
		`{"type":"activity","state":"unknown","at":"2026-05-27T19:00:00Z"}`,
		`{"type":"activity","state":"working","at":"2026-05-27T19:00:00Z"}`,
	} {
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Post(side.URL+"/event", "application/json", bytes.NewReader([]byte(ev)))
		if err != nil {
			t.Fatalf("post /event: %v", err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("forwarder status: got %d, want 204", resp.StatusCode)
		}
		_ = resp.Body.Close()
		select {
		case <-statusEvents:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("forward POST never reached CP")
		}
	}

	// Snapshot tick: pre-Cause-F the CP returned 400 here and silently
	// dropped the entire batch. The real handler now accepts 'unknown'.
	postMetricsSnapshot(ctx, cp.Client(), cfg, m, testLogger(slog.LevelInfo))

	unknownPts, err := metricsStore.RangeQuery(ctx, "ts:activity::alice:unknown", 0, 9999999999)
	if err != nil {
		t.Fatalf("RangeQuery unknown: %v", err)
	}
	if len(unknownPts) == 0 {
		t.Fatal("no 'unknown' activity points stored — CP rejected the batch (Cause F regression)")
	}

	all, err := stateChangeAcc.GetAll(ctx, "")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	sawWorkingTransition := false
	for _, c := range all {
		if c.ToState == "working" {
			sawWorkingTransition = true
		}
	}
	if !sawWorkingTransition {
		t.Errorf("expected transition into 'working' (from 'unknown') in accumulator; got %+v", all)
	}
}

// TestPostMetricsSnapshot_NoDebugLogs_WhenInfoLevel asserts the debug
// surface is fully off at info level — no leak of counts to production logs
// when the env var is unset.
func TestPostMetricsSnapshot_NoDebugLogs_WhenInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	cfg := config{AgentName: "alice", ControlPlaneURL: cp.URL}

	m := &Metrics{
		stateSecs:          map[string]float64{"working": 12.5},
		pendingTransitions: []stateChange{{From: "idle", To: "working", At: time.Now()}},
	}
	postMetricsSnapshot(context.Background(), cp.Client(), cfg, m, logger)

	if strings.Contains(buf.String(), "metrics snapshot:") {
		t.Errorf("snapshot debug logs should be suppressed at info level; got %q", buf.String())
	}
}
