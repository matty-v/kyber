package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/inbound"
)

// auditHarness wraps inboundHarness with a FakeRecorder + aggregator so
// audit-side tests can assert both Events and status.inboundRuns.
type auditHarness struct {
	*inboundHarness
	rec *record.FakeRecorder
	agg *api.InboundEventAggregator
}

// buildAuditHarness constructs the test rig with: a fake k8s client (Agent
// + Secret), inbound deduper / rate limiter / queue, a FakeRecorder, and
// the InboundEventAggregator wired up.
func buildAuditHarness(t *testing.T, cfg inboundHarnessConfig, objs ...runtime.Object) *auditHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()

	h := &inboundHarness{
		k8s:     fakeClient,
		jobDone: make(chan struct{}, 64),
	}

	handler := h.recorder()
	if cfg.blockingHandler {
		handler = func(_ context.Context, job inbound.Job) {
			h.jobsMu.Lock()
			h.jobs = append(h.jobs, job)
			h.jobsMu.Unlock()
			select {
			case h.jobDone <- struct{}{}:
			default:
			}
			if cfg.release != nil {
				<-cfg.release
			}
		}
	}

	rec := record.NewFakeRecorder(256)
	srv := &api.Server{
		K8sClient: fakeClient,
		APIKey:    testAPIKey,
		Namespace: "kyber-system",
		Recorder:  rec,
	}
	if !cfg.noDeduper {
		srv.InboundDeduper = inbound.NewMemoryDeduper()
	}
	if !cfg.noRateLimiter {
		srv.InboundRateLimiter = inbound.NewRateLimiter()
	}
	if !cfg.noQueue {
		h.queue = inbound.NewQueue(handler)
		srv.InboundQueue = h.queue
		t.Cleanup(func() { h.queue.Stop() })
	}

	agg := api.NewInboundEventAggregator(rec, fakeClient, "kyber-system")
	srv.InboundEventAggregator = agg
	t.Cleanup(func() { agg.Stop() })

	h.handler = srv.BuildHandler()
	return &auditHarness{inboundHarness: h, rec: rec, agg: agg}
}

// fetchAgent reads the latest Agent CR from the harness's fake client.
func (a *auditHarness) fetchAgent(t *testing.T, name string) *kyberv1.Agent {
	t.Helper()
	got := &kyberv1.Agent{}
	if err := a.k8s.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("get agent %q: %v", name, err)
	}
	return got
}

// drainEvents reads everything currently buffered in the FakeRecorder.
// FakeRecorder.Events is a buffered channel; this lets tests assert on
// the full set without racing the goroutine that emitted them.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestInboundAudit_Dispatched asserts a successful dispatch appends one
// "dispatched" entry to status.inboundRuns and emits one
// InboundDispatched Event.
func TestInboundAudit_Dispatched(t *testing.T) {
	const secret = "topsecret"
	body := validPushBody()
	sig := signBody([]byte(secret), body, "sha256=")

	h := buildAuditHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte(secret)),
	)

	rr := postInbound(t, h.inboundHarness, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	select {
	case <-h.jobDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("queue handler never fired")
	}

	got := h.fetchAgent(t, "dave")
	if len(got.Status.InboundRuns) != 1 {
		t.Fatalf("want 1 inbound run, got %d", len(got.Status.InboundRuns))
	}
	run := got.Status.InboundRuns[0]
	if run.Outcome != "dispatched" {
		t.Errorf("Outcome: got %q, want dispatched", run.Outcome)
	}
	if run.BindingName != "github" {
		t.Errorf("BindingName: got %q, want github", run.BindingName)
	}
	if run.RequestID == "" {
		t.Errorf("RequestID empty")
	}
	if run.DropReason != "" {
		t.Errorf("DropReason should be empty for dispatched, got %q", run.DropReason)
	}

	// At least one InboundDispatched Event should be in the channel.
	events := drainEvents(h.rec)
	if !containsEvent(events, "InboundDispatched") {
		t.Errorf("expected InboundDispatched event, got %v", events)
	}
}

// TestInboundAudit_SignatureMismatch: header present but wrong → status
// entry with DropReason=sig-mismatch + InboundAuthFailure Event.
func TestInboundAudit_SignatureMismatch(t *testing.T) {
	h := buildAuditHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte("topsecret")),
	)

	rr := postInbound(t, h.inboundHarness, "dave", "github", validPushBody(), map[string]string{
		"X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64),
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}

	got := h.fetchAgent(t, "dave")
	if len(got.Status.InboundRuns) != 1 {
		t.Fatalf("want 1 inbound run, got %d", len(got.Status.InboundRuns))
	}
	run := got.Status.InboundRuns[0]
	if run.Outcome != "dropped" {
		t.Errorf("Outcome: got %q, want dropped", run.Outcome)
	}
	if run.DropReason != "sig-mismatch" {
		t.Errorf("DropReason: got %q, want sig-mismatch", run.DropReason)
	}

	events := drainEvents(h.rec)
	if !containsEvent(events, "InboundAuthFailure") {
		t.Errorf("expected InboundAuthFailure event, got %v", events)
	}
}

// TestInboundAudit_NoEntryForProbeNoise asserts that the probe-noise
// outcomes (missing sig header, unknown agent, unknown binding, body too
// large) leave status.inboundRuns empty AND emit no Events.
func TestInboundAudit_NoEntryForProbeNoise(t *testing.T) {
	t.Run("missing signature header", func(t *testing.T) {
		h := buildAuditHarness(t, inboundHarnessConfig{},
			inboundAgent("dave", "github", "dave-github-hmac",
				"X-Hub-Signature-256", "sha256="),
			inboundSecret("dave-github-hmac", []byte("topsecret")),
		)
		rr := postInbound(t, h.inboundHarness, "dave", "github", validPushBody(), map[string]string{
			"X-GitHub-Event": "push",
		})
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rr.Code)
		}
		got := h.fetchAgent(t, "dave")
		if n := len(got.Status.InboundRuns); n != 0 {
			t.Errorf("missing-sig-header should not append; got %d entries", n)
		}
		if events := drainEvents(h.rec); len(events) != 0 {
			t.Errorf("missing-sig-header should not emit events; got %v", events)
		}
	})

	t.Run("unknown agent", func(t *testing.T) {
		h := buildAuditHarness(t, inboundHarnessConfig{}) // no objects
		rr := postInbound(t, h.inboundHarness, "ghost", "github", validPushBody(), map[string]string{
			"X-Hub-Signature-256": "sha256=00",
			"X-GitHub-Event":      "push",
		})
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rr.Code)
		}
		// No agent to fetch — just check no events emitted.
		if events := drainEvents(h.rec); len(events) != 0 {
			t.Errorf("unknown-agent should not emit events; got %v", events)
		}
	})

	t.Run("unknown binding", func(t *testing.T) {
		h := buildAuditHarness(t, inboundHarnessConfig{},
			inboundAgent("dave", "github", "dave-github-hmac",
				"X-Hub-Signature-256", "sha256="),
			inboundSecret("dave-github-hmac", []byte("topsecret")),
		)
		rr := postInbound(t, h.inboundHarness, "dave", "stripe", validPushBody(), map[string]string{
			"X-Hub-Signature-256": "sha256=00",
			"X-GitHub-Event":      "push",
		})
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rr.Code)
		}
		got := h.fetchAgent(t, "dave")
		if n := len(got.Status.InboundRuns); n != 0 {
			t.Errorf("unknown-binding should not append; got %d entries", n)
		}
		if events := drainEvents(h.rec); len(events) != 0 {
			t.Errorf("unknown-binding should not emit events; got %v", events)
		}
	})

	t.Run("body too large", func(t *testing.T) {
		h := buildAuditHarness(t, inboundHarnessConfig{},
			inboundAgent("dave", "github", "dave-github-hmac",
				"X-Hub-Signature-256", "sha256="),
			inboundSecret("dave-github-hmac", []byte("topsecret")),
		)
		big := bytes.Repeat([]byte("x"), 2*(1<<20))
		rr := postInbound(t, h.inboundHarness, "dave", "github", big, map[string]string{
			"X-Hub-Signature-256": "sha256=ignored",
			"X-GitHub-Event":      "push",
		})
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("want 413, got %d", rr.Code)
		}
		got := h.fetchAgent(t, "dave")
		if n := len(got.Status.InboundRuns); n != 0 {
			t.Errorf("body-too-large should not append; got %d entries", n)
		}
		if events := drainEvents(h.rec); len(events) != 0 {
			t.Errorf("body-too-large should not emit events; got %v", events)
		}
	})
}

// TestInboundAudit_RateLimitedAppendsAll asserts that when the rate
// limiter trips repeatedly, EVERY rate-limited request appears in the
// status ring buffer (capped at 50 per binding by the trim rules), and
// the aggregator sees one Increment per trip.
//
// Trip counts: with maxPerMinute=1 the FIRST request goes through (and
// dispatches), and the next 60 are rate-limited → 1 dispatched + 50
// rate-limited entries (after per-binding trim from 60 → 50).
func TestInboundAudit_RateLimitedAppendsAll(t *testing.T) {
	const secret = "topsecret"

	agent := inboundAgent("dave", "github", "dave-github-hmac",
		"X-Hub-Signature-256", "sha256=")
	agent.Spec.InboundBindings[0].Limits = &kyberv1.AgentInboundLimits{MaxPerMinute: 1}

	h := buildAuditHarness(t, inboundHarnessConfig{}, agent,
		inboundSecret("dave-github-hmac", []byte(secret)))

	postOne := func(i int) *httptest.ResponseRecorder {
		body := []byte(`{"repository":{"full_name":"matty-v/test"},"head_commit":{"message":"m` + strconv.Itoa(i) + `"}}`)
		return postInbound(t, h.inboundHarness, "dave", "github", body, map[string]string{
			"X-Hub-Signature-256": signBody([]byte(secret), body, "sha256="),
			"X-GitHub-Event":      "push",
		})
	}

	// First request: succeeds (consumes the single token).
	if rr := postOne(0); rr.Code != http.StatusOK {
		t.Fatalf("first POST want 200, got %d", rr.Code)
	}
	// Wait for the dispatch to land before flooding.
	select {
	case <-h.jobDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("first dispatch never fired")
	}

	// Next 60 should all be rate-limited.
	for i := 1; i <= 60; i++ {
		if rr := postOne(i); rr.Code != http.StatusTooManyRequests {
			t.Fatalf("POST %d want 429, got %d", i, rr.Code)
		}
	}

	got := h.fetchAgent(t, "dave")
	// Per-binding cap is MaxInboundRunsPerBinding = 50. We had 1
	// dispatched + 60 rate-limited = 61 total entries against the
	// "github" binding, all should compress down to 50 (newest-50).
	if got := len(got.Status.InboundRuns); got != 50 {
		t.Errorf("want 50 entries (per-binding cap), got %d", got)
	}
	// Count rate-limited entries that survived the trim. The newest 50
	// entries are all dropped/rate-limited (the dispatched entry was
	// the oldest and got trimmed first).
	rateLimited := 0
	dispatched := 0
	for _, r := range got.Status.InboundRuns {
		switch r.DropReason {
		case "rate-limited":
			rateLimited++
		}
		if r.Outcome == "dispatched" {
			dispatched++
		}
	}
	if rateLimited != 50 {
		t.Errorf("expected 50 rate-limited entries after trim, got %d (dispatched=%d)", rateLimited, dispatched)
	}

	// The aggregator should have one bucket with count=60.
	h.agg.FlushNowForTest()
	events := drainEvents(h.rec)
	rateLimitTrips := 0
	for _, e := range events {
		if strings.Contains(e, "RateLimitTripped") && strings.Contains(e, "60 in last") {
			rateLimitTrips++
		}
	}
	if rateLimitTrips != 1 {
		t.Errorf("expected 1 aggregated RateLimitTripped event with count=60, got %d (events=%v)", rateLimitTrips, events)
	}
}

// TestInboundAudit_PerBindingCap asserts the cap is per-binding: 60
// dispatches against "foo" + 30 against "bar" → 50 foo + 30 bar.
//
// Drives the queue serially (post → wait for jobDone → next post) so the
// QueueDepth=5 buffer never saturates. Trying to flood the receiver
// without a synchronization barrier would cause some POSTs to hit
// queue-full and produce different status entries than this test
// asserts on.
func TestInboundAudit_PerBindingCap(t *testing.T) {
	const secret = "topsecret"

	// Build an agent with two bindings, both pointing at the same
	// shared HMAC secret (simplifies the test; in production they'd
	// differ).
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine:   "worker-1",
			Runtime:   "claude-code",
			Model:     "claude-sonnet-4",
			Scaling:   kyberv1.AgentScalingWarm,
			Resources: kyberv1.AgentResources{},
			Secrets:   kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			InboundBindings: []kyberv1.AgentInboundBinding{
				{
					Name:            "foo",
					ExistingSecret:  "shared-hmac",
					SignatureHeader: "X-Hub-Signature-256",
					SignaturePrefix: "sha256=",
					EventHeader:     "X-GitHub-Event",
					MatchEvents:     []string{"push"},
					Action:          "Investigate.",
					Limits:          &kyberv1.AgentInboundLimits{MaxPerMinute: 600},
				},
				{
					Name:            "bar",
					ExistingSecret:  "shared-hmac",
					SignatureHeader: "X-Hub-Signature-256",
					SignaturePrefix: "sha256=",
					EventHeader:     "X-GitHub-Event",
					MatchEvents:     []string{"push"},
					Action:          "Investigate.",
					Limits:          &kyberv1.AgentInboundLimits{MaxPerMinute: 600},
				},
			},
		},
	}

	h := buildAuditHarness(t, inboundHarnessConfig{}, agent,
		inboundSecret("shared-hmac", []byte(secret)))

	postBinding := func(binding string, i int) {
		body := []byte(`{"repository":{"full_name":"matty-v/x"},"head_commit":{"message":"m` + binding + strconv.Itoa(i) + `"}}`)
		rr := postInbound(t, h.inboundHarness, "dave", binding, body, map[string]string{
			"X-Hub-Signature-256": signBody([]byte(secret), body, "sha256="),
			"X-GitHub-Event":      "push",
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s/%d want 200, got %d: %s", binding, i, rr.Code, rr.Body.String())
		}
		// Wait for the job to drain through the queue before posting the
		// next one — the buffered channel has depth 5 and the test would
		// otherwise hit queue-full.
		select {
		case <-h.jobDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("dispatch %s/%d never landed in handler", binding, i)
		}
	}

	for i := 0; i < 60; i++ {
		postBinding("foo", i)
	}
	for i := 0; i < 30; i++ {
		postBinding("bar", i)
	}

	got := h.fetchAgent(t, "dave")
	foo := 0
	bar := 0
	for _, r := range got.Status.InboundRuns {
		switch r.BindingName {
		case "foo":
			foo++
		case "bar":
			bar++
		}
	}
	if foo != 50 {
		t.Errorf("foo entries: got %d, want 50 (per-binding cap)", foo)
	}
	if bar != 30 {
		t.Errorf("bar entries: got %d, want 30 (under cap)", bar)
	}
}

// containsEvent returns true if any event in events contains needle.
// FakeRecorder formats events as "TYPE REASON MESSAGE", so a substring
// match on the reason works.
func containsEvent(events []string, needle string) bool {
	for _, e := range events {
		if strings.Contains(e, needle) {
			return true
		}
	}
	return false
}

// ====================================================================
// InboundEventAggregator unit tests.
// ====================================================================

// TestInboundEventAggregator_FlushBatches asserts 10 increments on the
// same key produce one Event with count=10 in its message.
func TestInboundEventAggregator_FlushBatches(t *testing.T) {
	scheme := mustNewScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
	rec := record.NewFakeRecorder(16)

	agg := api.NewInboundEventAggregator(rec, c, "kyber-system")
	t.Cleanup(agg.Stop)

	for i := 0; i < 10; i++ {
		agg.Increment("dave", "RateLimitTripped", "rate limit exceeded")
	}
	agg.FlushNowForTest()

	events := drainEvents(rec)
	if len(events) != 1 {
		t.Fatalf("want 1 aggregated event, got %d: %v", len(events), events)
	}
	if !strings.Contains(events[0], "RateLimitTripped") {
		t.Errorf("event missing reason: %q", events[0])
	}
	if !strings.Contains(events[0], "10 in last") {
		t.Errorf("event missing count: %q", events[0])
	}
}

// TestInboundEventAggregator_KeysIsolated asserts that distinct
// (agent, eventType, reason) tuples produce distinct Events with
// independent counts.
func TestInboundEventAggregator_KeysIsolated(t *testing.T) {
	scheme := mustNewScheme(t)
	agentA := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agentA", Namespace: "kyber-system"}}
	agentB := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agentB", Namespace: "kyber-system"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agentA, agentB).Build()
	rec := record.NewFakeRecorder(16)

	agg := api.NewInboundEventAggregator(rec, c, "kyber-system")
	t.Cleanup(agg.Stop)

	agg.Increment("agentA", "RateLimitTripped", "rate limit exceeded on binding=foo")
	agg.Increment("agentA", "RateLimitTripped", "rate limit exceeded on binding=foo")
	agg.Increment("agentB", "RateLimitTripped", "rate limit exceeded on binding=foo")
	agg.Increment("agentA", "QueueFull", "inbound queue saturated on binding=foo")

	agg.FlushNowForTest()
	events := drainEvents(rec)
	if len(events) != 3 {
		t.Fatalf("want 3 distinct events, got %d: %v", len(events), events)
	}

	// Verify each expected combination is present with the right count.
	var sawAFooRL, sawBFooRL, sawAQueue bool
	for _, e := range events {
		if strings.Contains(e, "RateLimitTripped") && strings.Contains(e, "2 in last") {
			sawAFooRL = true
		}
		if strings.Contains(e, "RateLimitTripped") && strings.Contains(e, "1 in last") {
			sawBFooRL = true
		}
		if strings.Contains(e, "QueueFull") && strings.Contains(e, "1 in last") {
			sawAQueue = true
		}
	}
	if !sawAFooRL || !sawBFooRL || !sawAQueue {
		t.Errorf("missing one or more expected events: aFooRL=%v bFooRL=%v aQueue=%v events=%v",
			sawAFooRL, sawBFooRL, sawAQueue, events)
	}
}

// TestInboundEventAggregator_StopFlushesPending asserts that pending
// counts are flushed when Stop() is called, not just dropped on the floor.
func TestInboundEventAggregator_StopFlushesPending(t *testing.T) {
	scheme := mustNewScheme(t)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber-system"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
	rec := record.NewFakeRecorder(16)

	agg := api.NewInboundEventAggregator(rec, c, "kyber-system")
	for i := 0; i < 5; i++ {
		agg.Increment("dave", "QueueFull", "queue saturated")
	}
	agg.Stop()

	events := drainEvents(rec)
	if len(events) != 1 {
		t.Fatalf("want 1 final flush event, got %d: %v", len(events), events)
	}
	if !strings.Contains(events[0], "QueueFull") || !strings.Contains(events[0], "5 in last") {
		t.Errorf("final flush event wrong: %q", events[0])
	}
}

// TestInboundEventAggregator_NilRecorder asserts Increment is a no-op
// when the recorder is nil — preserves the "missing dep doesn't black-
// hole traffic" invariant.
func TestInboundEventAggregator_NilRecorder(t *testing.T) {
	scheme := mustNewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	agg := api.NewInboundEventAggregator(nil, c, "kyber-system")
	t.Cleanup(agg.Stop)

	// Should not panic.
	agg.Increment("dave", "QueueFull", "queue saturated")
	agg.FlushNowForTest()
}

// Make sure the corev1 / metav1 imports are kept used even if the file
// shape changes — these are referenced by helpers and by future tests.
var _ = corev1.EventTypeWarning
var _ = metav1.NewTime
