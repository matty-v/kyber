package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/inbound"
)

// flakyEnvelopeCache wraps a real cache but replaces Get with a forced
// error. Used for the cache-blip test path.
type flakyEnvelopeCache struct {
	getErr error
}

func (f *flakyEnvelopeCache) Put(_ context.Context, _ string, _ string) error { return nil }
func (f *flakyEnvelopeCache) Get(_ context.Context, _ string) (string, error) {
	return "", f.getErr
}

// replayHarness wires the Server with an EnvelopeCache + Queue so a test
// can drive POST .../replay/{requestId} end-to-end.
type replayHarness struct {
	handler http.Handler
	srv     *api.Server
	queue   *inbound.Queue
	cache   inbound.EnvelopeCache

	jobsMu sync.Mutex
	jobs   []inbound.Job
	jobCh  chan struct{}
}

func (h *replayHarness) recordedJobs() []inbound.Job {
	h.jobsMu.Lock()
	defer h.jobsMu.Unlock()
	out := make([]inbound.Job, len(h.jobs))
	copy(out, h.jobs)
	return out
}

type replayHarnessOpts struct {
	// cache overrides the default in-memory cache. Use for the
	// service-unavailable and cache-error paths.
	cache inbound.EnvelopeCache
	// noCache nils the cache slot entirely (test the 503 path).
	noCache bool
	// queueDepthZero forces the test to saturate the queue with a
	// blocking handler before calling replay.
	blockingHandler bool
	// release is closed by the test to free a blocking handler.
	release chan struct{}
}

func buildReplayHarness(t *testing.T, opts replayHarnessOpts, objs ...runtime.Object) *replayHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()

	h := &replayHarness{jobCh: make(chan struct{}, 32)}
	handler := func(_ context.Context, job inbound.Job) {
		h.jobsMu.Lock()
		h.jobs = append(h.jobs, job)
		h.jobsMu.Unlock()
		select {
		case h.jobCh <- struct{}{}:
		default:
		}
		if opts.blockingHandler && opts.release != nil {
			<-opts.release
		}
	}
	h.queue = inbound.NewQueue(handler)
	t.Cleanup(func() { h.queue.Stop() })

	srv := &api.Server{
		K8sClient:    fakeClient,
		APIKey:       testAPIKey,
		Namespace:    "kyber-system",
		InboundQueue: h.queue,
	}
	if !opts.noCache {
		if opts.cache != nil {
			h.cache = opts.cache
		} else {
			h.cache = inbound.NewMemoryEnvelopeCache()
		}
		srv.InboundEnvelopeCache = h.cache
	}
	h.srv = srv
	h.handler = srv.BuildHandler()
	return h
}

// postReplay POSTs to the replay endpoint with the test API key.
func postReplay(t *testing.T, h *replayHarness, agent, binding, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/agents/" + agent + "/inbound-bindings/" + binding + "/replay/" + requestID
	req := httptest.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

// agentWithBinding returns a minimal Agent CRD carrying one named binding.
func agentWithBinding(agentName, bindingName string) *kyberv1.Agent {
	a := bareAgent(agentName)
	a.Spec.InboundBindings = []kyberv1.AgentInboundBinding{
		{
			Name:            bindingName,
			ExistingSecret:  agentName + "-" + bindingName + "-hmac",
			SignatureHeader: "X-Hub-Signature-256",
			SignaturePrefix: "sha256=",
			EventHeader:     "X-GitHub-Event",
			MatchEvents:     []string{"push"},
			Action:          "investigate",
		},
	}
	return a
}

// TestReplay_HappyPath: cache holds envelope → 202, queue gets the job,
// status ring buffer carries a "replay of …" note.
func TestReplay_HappyPath(t *testing.T) {
	h := buildReplayHarness(t, replayHarnessOpts{},
		agentWithBinding("dave", "github"))

	// Pre-stash an envelope as if the receiver had written it.
	envelope := "[inbound:original-1] binding=github agent=dave\nORIGINAL\n"
	if err := h.cache.Put(context.Background(), "original-1", envelope); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	rr := postReplay(t, h, "dave", "github", "original-1")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		OriginalRequestID string `json:"originalRequestId"`
		NewRequestID      string `json:"newRequestId"`
		Status            string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	if resp.OriginalRequestID != "original-1" {
		t.Errorf("originalRequestId: got %q", resp.OriginalRequestID)
	}
	if resp.NewRequestID == "" || resp.NewRequestID == resp.OriginalRequestID {
		t.Errorf("newRequestId: should be a fresh UUID, got %q", resp.NewRequestID)
	}
	if resp.Status != "queued" {
		t.Errorf("status: got %q want %q", resp.Status, "queued")
	}

	// Wait for the queue worker to deliver.
	select {
	case <-h.jobCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("queue handler never fired")
	}
	jobs := h.recordedJobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	if jobs[0].Envelope != envelope {
		t.Errorf("queue job envelope mismatch:\n got %q\n want %q", jobs[0].Envelope, envelope)
	}
	if jobs[0].RequestID != resp.NewRequestID {
		t.Errorf("queue job request id mismatch: got %q want %q",
			jobs[0].RequestID, resp.NewRequestID)
	}

	// Status ring buffer should have one entry referencing the original.
	a := &kyberv1.Agent{}
	if err := h.srv.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: "dave", Namespace: "kyber-system"}, a); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(a.Status.InboundRuns) != 1 {
		t.Fatalf("expected 1 status run, got %d", len(a.Status.InboundRuns))
	}
	run := a.Status.InboundRuns[0]
	if run.Outcome != "dispatched" {
		t.Errorf("outcome: got %q want %q", run.Outcome, "dispatched")
	}
	if !strings.Contains(run.Error, "replay of original-1") {
		t.Errorf("status entry should note replay; got error=%q", run.Error)
	}
}

// TestReplay_EnvelopeExpired: cache returns "" → 410 with the documented
// envelope_expired body shape.
func TestReplay_EnvelopeExpired(t *testing.T) {
	h := buildReplayHarness(t, replayHarnessOpts{},
		agentWithBinding("dave", "github"))
	// Note: nothing put in the cache.

	rr := postReplay(t, h, "dave", "github", "long-gone")
	if rr.Code != http.StatusGone {
		t.Fatalf("want 410, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if body.Error != "envelope_expired" {
		t.Errorf("error: got %q want envelope_expired", body.Error)
	}
	if !strings.Contains(body.Message, "7-day") {
		t.Errorf("message should reference the 7-day cache; got %q", body.Message)
	}
	if got := len(h.recordedJobs()); got != 0 {
		t.Errorf("no jobs should have been queued, got %d", got)
	}
}

// TestReplay_CacheError: cache.Get returns an error → 503.
func TestReplay_CacheError(t *testing.T) {
	flaky := &flakyEnvelopeCache{getErr: errors.New("redis: connection refused")}
	h := buildReplayHarness(t, replayHarnessOpts{cache: flaky},
		agentWithBinding("dave", "github"))

	rr := postReplay(t, h, "dave", "github", "any-id")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestReplay_UnknownBinding: agent exists but no such binding → 404.
func TestReplay_UnknownBinding(t *testing.T) {
	h := buildReplayHarness(t, replayHarnessOpts{},
		agentWithBinding("dave", "github"))
	if err := h.cache.Put(context.Background(), "some-id", "envelope"); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	rr := postReplay(t, h, "dave", "stripe", "some-id")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestReplay_UnknownAgent: agent doesn't exist → 404.
func TestReplay_UnknownAgent(t *testing.T) {
	h := buildReplayHarness(t, replayHarnessOpts{})

	rr := postReplay(t, h, "ghost", "github", "any-id")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestReplay_QueueFull: per-agent queue saturated → 429.
func TestReplay_QueueFull(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	h := buildReplayHarness(t, replayHarnessOpts{
		blockingHandler: true,
		release:         release,
	}, agentWithBinding("dave", "github"))

	envelope := "envelope-x"
	if err := h.cache.Put(context.Background(), "original-1", envelope); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Fill the per-agent queue: QueueDepth=5 plus one in flight on the
	// blocking handler. The cache only has one envelope, so we directly
	// enqueue jobs with the queue's Enqueue to fill it without going
	// through the API.
	//
	// Send one job and WAIT for the worker to receive it before sending
	// the rest — without this wait, the test races the goroutine
	// scheduler: if the worker hasn't pulled job#1 by the time we send
	// jobs 2..6, the buffer is full at 5/5 and Enqueue#6 returns
	// ErrQueueFull (silently — we discard the err); but if the worker
	// pulls between sends, the buffer absorbs all 6 and the replay
	// happens to find a free slot, so the assertion fails.
	if err := h.queue.Enqueue(inbound.Job{
		Agent: "dave", Binding: "github", RequestID: "filler-1",
		Envelope: envelope, EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed enqueue: %v", err)
	}
	select {
	case <-h.jobCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("worker never picked up the first job")
	}
	// Worker is now blocked in the handler; send QueueDepth more jobs
	// to fill the channel buffer to capacity.
	for i := 0; i < inbound.QueueDepth; i++ {
		_ = h.queue.Enqueue(inbound.Job{
			Agent: "dave", Binding: "github", RequestID: "filler",
			Envelope: envelope, EnqueuedAt: time.Now(),
		})
	}

	// Now a replay should bounce because the queue is saturated.
	rr := postReplay(t, h, "dave", "github", "original-1")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestReplay_NoCacheConfigured: server has no envelope cache wired → 503.
func TestReplay_NoCacheConfigured(t *testing.T) {
	h := buildReplayHarness(t, replayHarnessOpts{noCache: true},
		agentWithBinding("dave", "github"))

	rr := postReplay(t, h, "dave", "github", "any-id")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestReplay_MissingAuth: no API key → 401 (handled by the auth middleware,
// not the handler).
func TestReplay_MissingAuth(t *testing.T) {
	h := buildReplayHarness(t, replayHarnessOpts{},
		agentWithBinding("dave", "github"))
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/dave/inbound-bindings/github/replay/x", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 (no API key), got %d", rr.Code)
	}
}

// TestReplay_GETIs405: only POST is accepted.
func TestReplay_GETIs405(t *testing.T) {
	h := buildReplayHarness(t, replayHarnessOpts{},
		agentWithBinding("dave", "github"))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agents/dave/inbound-bindings/github/replay/x", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestReplay_MissingRequestID: /replay with empty requestId → 400. We
// hit this through /replay/ which the dispatcher must reject before
// looking at the cache.
func TestReplay_MissingRequestID(t *testing.T) {
	h := buildReplayHarness(t, replayHarnessOpts{},
		agentWithBinding("dave", "github"))
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/dave/inbound-bindings/github/replay/", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

