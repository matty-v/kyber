package api_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/inbound"
)

// inboundHarness wires the bits a /webhooks/inbound test cares about: the
// HTTP handler, the fake k8s client (to seed Agents/Secrets), and a Job
// recorder that captures every queue dispatch so a test can assert the
// envelope reached the queue.
type inboundHarness struct {
	handler http.Handler
	k8s     client.Client

	// jobsMu / jobs accumulate Job entries seen by the queue handler.
	// Mu is needed because the queue worker dispatches jobs from a
	// goroutine and the test asserts from the main goroutine.
	jobsMu sync.Mutex
	jobs   []inbound.Job

	// queue is exposed so individual tests can configure a tiny queue
	// (e.g. saturate it for the queue-full case).
	queue *inbound.Queue

	// jobDone is closed (drained) every time a job hits the recorder so
	// the test can wait deterministically rather than time.Sleep.
	jobDone chan struct{}
}

// recorder returns the queue handler closure that appends each job to the
// harness's jobs slice and signals jobDone.
func (h *inboundHarness) recorder() inbound.Handler {
	return func(_ context.Context, job inbound.Job) {
		h.jobsMu.Lock()
		h.jobs = append(h.jobs, job)
		h.jobsMu.Unlock()
		select {
		case h.jobDone <- struct{}{}:
		default:
		}
	}
}

// recordedJobs returns a snapshot of the jobs seen so far.
func (h *inboundHarness) recordedJobs() []inbound.Job {
	h.jobsMu.Lock()
	defer h.jobsMu.Unlock()
	out := make([]inbound.Job, len(h.jobs))
	copy(out, h.jobs)
	return out
}

// inboundHarnessConfig customises a harness without making the constructor
// signature unwieldy.
type inboundHarnessConfig struct {
	// noDeduper omits the Deduper entirely (tests that don't care).
	noDeduper bool
	// noRateLimiter omits the RateLimiter (tests that don't care).
	noRateLimiter bool
	// noQueue omits the Queue (used to assert the 503 path implicitly —
	// none of the current cases need this, but the field is here for
	// future tests).
	noQueue bool
	// blockingHandler turns the queue handler into one that blocks on
	// release until the test signals — used to saturate the queue
	// without races.
	blockingHandler bool
	// release is signalled by the test to unblock the blocking handler.
	release chan struct{}
	// envelopeCache, when non-nil, is wired onto Server.InboundEnvelopeCache.
	// Used by Phase 3 tests that assert the receiver writes the rendered
	// envelope to the cache before queueing.
	envelopeCache inbound.EnvelopeCache
}

func buildInboundHarness(t *testing.T, cfg inboundHarnessConfig, objs ...runtime.Object) *inboundHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()

	h := &inboundHarness{
		k8s:     fakeClient,
		jobDone: make(chan struct{}, 32),
	}

	handler := h.recorder()
	if cfg.blockingHandler {
		// Blocking variant: append the job, signal jobDone, then wait
		// on release before returning so the queue's per-agent worker
		// holds onto the slot.
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

	srv := &api.Server{
		K8sClient: fakeClient,
		APIKey:    testAPIKey,
		Namespace: "kyber-system",
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
	if cfg.envelopeCache != nil {
		srv.InboundEnvelopeCache = cfg.envelopeCache
	}

	h.handler = srv.BuildHandler()
	return h
}

// inboundAgent returns an Agent CRD with one inbound binding suitable for
// happy-path tests.
func inboundAgent(name, bindingName, secretName, sigHeader, sigPrefix string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Scaling: kyberv1.AgentScalingWarm,
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			InboundBindings: []kyberv1.AgentInboundBinding{
				{
					Name:            bindingName,
					ExistingSecret:  secretName,
					SignatureHeader: sigHeader,
					SignaturePrefix: sigPrefix,
					EventHeader:     "X-GitHub-Event",
					MatchEvents:     []string{"push"},
					Filters: []kyberv1.AgentInboundFilter{
						{JsonPath: "$.repository.full_name", Equals: "matty-v/test"},
					},
					Fields: []kyberv1.AgentInboundField{
						{Label: "repo", JsonPath: "$.repository.full_name"},
						{Label: "head", JsonPath: "$.head_commit.message", Truncate: 100},
					},
					Action: "Investigate the push event.",
					Limits: &kyberv1.AgentInboundLimits{MaxPerMinute: 10},
				},
			},
		},
	}
}

// inboundSecret returns a corev1.Secret with the conventional webhook-secret
// key used by the inbound binding.
func inboundSecret(name string, secret []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Data:       map[string][]byte{"webhook-secret": secret},
	}
}

// signBody returns the hex-encoded HMAC-SHA256 of body, optionally prefixed.
func signBody(secret, body []byte, prefix string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return prefix + hex.EncodeToString(mac.Sum(nil))
}

// validPushBody is a tiny GitHub-style push event the binding above accepts.
func validPushBody() []byte {
	return []byte(`{"repository":{"full_name":"matty-v/test"},"head_commit":{"message":"first commit"}}`)
}

// postInbound builds and sends a request to /webhooks/inbound/<agent>/<binding>.
func postInbound(t *testing.T, h *inboundHarness, agent, binding string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/webhooks/inbound/"+agent+"/"+binding, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

// assertUnauthorizedBody checks the body matches exactly the canonical 401
// shape — no leak of which check failed.
func assertUnauthorizedBody(t *testing.T, body string) {
	t.Helper()
	want := `{"error":"unauthorized"}`
	got := strings.TrimRight(body, "\n")
	if got != want {
		t.Errorf("401 body: got %q, want %q", got, want)
	}
}

// TestInboundHappyPath: signed, matched event, valid filter — queues a job.
func TestInboundHappyPath(t *testing.T) {
	const secret = "topsecret"
	body := validPushBody()
	sig := signBody([]byte(secret), body, "sha256=")

	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte(secret)),
	)

	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
		"Content-Type":        "application/json",
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Inbound-Request-Id") == "" {
		t.Errorf("expected X-Inbound-Request-Id header to be set")
	}

	// Wait for the queue worker to deliver the job.
	select {
	case <-h.jobDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("queue handler never fired")
	}

	jobs := h.recordedJobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	if jobs[0].Agent != "dave" || jobs[0].Binding != "github" {
		t.Errorf("job agent/binding: got %s/%s", jobs[0].Agent, jobs[0].Binding)
	}
	if !strings.Contains(jobs[0].Envelope, "matty-v/test") {
		t.Errorf("envelope missing repo data: %s", jobs[0].Envelope)
	}
	if !strings.Contains(jobs[0].Envelope, "Investigate the push event.") {
		t.Errorf("envelope missing action: %s", jobs[0].Envelope)
	}
}

// TestInboundMissingSignatureHeader: no X-Hub-Signature-256 header → 401 generic.
func TestInboundMissingSignatureHeader(t *testing.T) {
	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte("topsecret")),
	)

	rr := postInbound(t, h, "dave", "github", validPushBody(), map[string]string{
		"X-GitHub-Event": "push",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", rr.Code, rr.Body.String())
	}
	assertUnauthorizedBody(t, rr.Body.String())
}

// TestInboundWrongSignature: header present but wrong → 401 generic.
func TestInboundWrongSignature(t *testing.T) {
	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte("topsecret")),
	)

	rr := postInbound(t, h, "dave", "github", validPushBody(), map[string]string{
		"X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64),
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", rr.Code, rr.Body.String())
	}
	assertUnauthorizedBody(t, rr.Body.String())
}

// TestInboundUnknownAgent: agent in URL doesn't exist in cluster.
func TestInboundUnknownAgent(t *testing.T) {
	h := buildInboundHarness(t, inboundHarnessConfig{}) // no agents seeded.

	body := validPushBody()
	sig := signBody([]byte("anything"), body, "sha256=")
	rr := postInbound(t, h, "ghost", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	assertUnauthorizedBody(t, rr.Body.String())
}

// TestInboundUnknownBinding: agent exists but no binding by that name.
func TestInboundUnknownBinding(t *testing.T) {
	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte("topsecret")),
	)

	body := validPushBody()
	sig := signBody([]byte("topsecret"), body, "sha256=")
	rr := postInbound(t, h, "dave", "stripe", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	assertUnauthorizedBody(t, rr.Body.String())
}

// TestInboundMissingSecret: binding references a Secret that does not exist.
func TestInboundMissingSecret(t *testing.T) {
	// Note: agent seeded with no Secret object.
	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
	)

	body := validPushBody()
	sig := signBody([]byte("anything"), body, "sha256=")
	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	assertUnauthorizedBody(t, rr.Body.String())
}

// TestInboundBodyTooLarge: 2 MiB body → 413.
func TestInboundBodyTooLarge(t *testing.T) {
	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte("topsecret")),
	)

	big := bytes.Repeat([]byte("x"), 2*(1<<20))
	rr := postInbound(t, h, "dave", "github", big, map[string]string{
		"X-Hub-Signature-256": "sha256=ignored",
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestInboundDedup: same signed body twice within TTL → second is "deduped"
// 200 (no second job dispatched).
func TestInboundDedup(t *testing.T) {
	const secret = "topsecret"
	body := validPushBody()
	sig := signBody([]byte(secret), body, "sha256=")

	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte(secret)),
	)

	hdrs := map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	}

	rr := postInbound(t, h, "dave", "github", body, hdrs)
	if rr.Code != http.StatusOK {
		t.Fatalf("first POST want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// Wait for the first job to flush.
	select {
	case <-h.jobDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("first POST never dispatched")
	}

	rr2 := postInbound(t, h, "dave", "github", body, hdrs)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second POST want 200 (deduped), got %d", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "deduped") {
		t.Errorf("second POST should report deduped status, got %s", rr2.Body.String())
	}

	// Give the queue a moment to (incorrectly) deliver a second job, then
	// confirm we still only have 1.
	select {
	case <-h.jobDone:
		t.Fatalf("dedup leak: second job was dispatched anyway")
	case <-time.After(100 * time.Millisecond):
	}
	if got := len(h.recordedJobs()); got != 1 {
		t.Errorf("want 1 dispatched job, got %d", got)
	}
}

// TestInboundUnmatchedEvent: event header doesn't match matchEvents → 200 dropped.
func TestInboundUnmatchedEvent(t *testing.T) {
	const secret = "topsecret"
	body := validPushBody()
	sig := signBody([]byte(secret), body, "sha256=")

	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte(secret)),
	)

	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "ping", // not in matchEvents.
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 (dropped), got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "dropped") {
		t.Errorf("expected dropped status: %s", rr.Body.String())
	}
	if got := len(h.recordedJobs()); got != 0 {
		t.Errorf("dropped event reached the queue: %d jobs", got)
	}
}

// TestInboundFilterRejected: matched event but filter excludes the body.
func TestInboundFilterRejected(t *testing.T) {
	const secret = "topsecret"
	body := []byte(`{"repository":{"full_name":"someone-else/repo"},"head_commit":{"message":"x"}}`)
	sig := signBody([]byte(secret), body, "sha256=")

	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte(secret)),
	)

	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 (dropped), got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "filter-rejected") {
		t.Errorf("expected filter-rejected drop reason: %s", rr.Body.String())
	}
}

// TestInboundRateLimited: maxPerMinute=1, fire two — second is 429.
func TestInboundRateLimited(t *testing.T) {
	const secret = "topsecret"

	agent := inboundAgent("dave", "github", "dave-github-hmac",
		"X-Hub-Signature-256", "sha256=")
	agent.Spec.InboundBindings[0].Limits = &kyberv1.AgentInboundLimits{MaxPerMinute: 1}

	h := buildInboundHarness(t, inboundHarnessConfig{}, agent,
		inboundSecret("dave-github-hmac", []byte(secret)))

	// Two distinct bodies so dedup doesn't shadow the rate-limit path.
	bodyA := []byte(`{"repository":{"full_name":"matty-v/test"},"head_commit":{"message":"a"}}`)
	bodyB := []byte(`{"repository":{"full_name":"matty-v/test"},"head_commit":{"message":"b"}}`)

	for _, body := range [][]byte{bodyA} {
		rr := postInbound(t, h, "dave", "github", body, map[string]string{
			"X-Hub-Signature-256": signBody([]byte(secret), body, "sha256="),
			"X-GitHub-Event":      "push",
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("first POST want 200, got %d", rr.Code)
		}
	}

	rr := postInbound(t, h, "dave", "github", bodyB, map[string]string{
		"X-Hub-Signature-256": signBody([]byte(secret), bodyB, "sha256="),
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second POST want 429, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestInboundQueueFull: with a saturated queue + blocking handler, the next
// Enqueue returns ErrQueueFull → 429.
func TestInboundQueueFull(t *testing.T) {
	const secret = "topsecret"

	// Use a large rate limit so this test isolates queue saturation.
	agent := inboundAgent("dave", "github", "dave-github-hmac",
		"X-Hub-Signature-256", "sha256=")
	agent.Spec.InboundBindings[0].Limits = &kyberv1.AgentInboundLimits{MaxPerMinute: 600}

	release := make(chan struct{})
	h := buildInboundHarness(t, inboundHarnessConfig{
		blockingHandler: true,
		release:         release,
	}, agent, inboundSecret("dave-github-hmac", []byte(secret)))

	// QueueDepth=5 (constant), so 1 in-flight + 5 buffered = 6 accepted
	// before the 7th hits ErrQueueFull. Each request needs a unique body
	// to bypass dedup.
	postOne := func(i int) *httptest.ResponseRecorder {
		body := []byte(`{"repository":{"full_name":"matty-v/test"},"head_commit":{"message":"m` + strconv.Itoa(i) + `"}}`)
		return postInbound(t, h, "dave", "github", body, map[string]string{
			"X-Hub-Signature-256": signBody([]byte(secret), body, "sha256="),
			"X-GitHub-Event":      "push",
		})
	}

	// Fill: first request enters the worker (which blocks), next 5 buffer.
	for i := 0; i < 6; i++ {
		rr := postOne(i)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %d expected 200 while filling, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}
	// Wait for at least one job to land in the recorder so we know the
	// worker is parked on release.
	select {
	case <-h.jobDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("blocking handler never picked up the first job")
	}

	// 7th must 429 with queue-full.
	rr := postOne(6)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("7th POST want 429 (queue full), got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "queue_full") {
		t.Errorf("expected queue_full code in body: %s", rr.Body.String())
	}

	// Release so the queue worker can drain on cleanup.
	close(release)
}

// TestInboundUnauthorizedBodyShape: the canonical 401 body is identical for
// every auth/binding/secret failure mode — no info leak.
func TestInboundUnauthorizedBodyShape(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (*inboundHarness, *http.Request)
	}{
		{
			name: "no signature header",
			setup: func(t *testing.T) (*inboundHarness, *http.Request) {
				h := buildInboundHarness(t, inboundHarnessConfig{},
					inboundAgent("dave", "github", "dave-github-hmac",
						"X-Hub-Signature-256", "sha256="),
					inboundSecret("dave-github-hmac", []byte("s")))
				req := httptest.NewRequest(http.MethodPost,
					"/webhooks/inbound/dave/github", bytes.NewReader(validPushBody()))
				req.Header.Set("X-GitHub-Event", "push")
				return h, req
			},
		},
		{
			name: "agent missing",
			setup: func(t *testing.T) (*inboundHarness, *http.Request) {
				h := buildInboundHarness(t, inboundHarnessConfig{})
				req := httptest.NewRequest(http.MethodPost,
					"/webhooks/inbound/ghost/github", bytes.NewReader(validPushBody()))
				req.Header.Set("X-Hub-Signature-256", "sha256=00")
				return h, req
			},
		},
		{
			name: "binding missing",
			setup: func(t *testing.T) (*inboundHarness, *http.Request) {
				h := buildInboundHarness(t, inboundHarnessConfig{},
					inboundAgent("dave", "github", "dave-github-hmac",
						"X-Hub-Signature-256", "sha256="),
					inboundSecret("dave-github-hmac", []byte("s")))
				req := httptest.NewRequest(http.MethodPost,
					"/webhooks/inbound/dave/missing", bytes.NewReader(validPushBody()))
				req.Header.Set("X-Hub-Signature-256", "sha256=00")
				return h, req
			},
		},
		{
			name: "secret missing",
			setup: func(t *testing.T) (*inboundHarness, *http.Request) {
				h := buildInboundHarness(t, inboundHarnessConfig{},
					inboundAgent("dave", "github", "dave-github-hmac",
						"X-Hub-Signature-256", "sha256="))
				req := httptest.NewRequest(http.MethodPost,
					"/webhooks/inbound/dave/github", bytes.NewReader(validPushBody()))
				req.Header.Set("X-Hub-Signature-256", "sha256=00")
				return h, req
			},
		},
		{
			name: "wrong signature",
			setup: func(t *testing.T) (*inboundHarness, *http.Request) {
				h := buildInboundHarness(t, inboundHarnessConfig{},
					inboundAgent("dave", "github", "dave-github-hmac",
						"X-Hub-Signature-256", "sha256="),
					inboundSecret("dave-github-hmac", []byte("s")))
				req := httptest.NewRequest(http.MethodPost,
					"/webhooks/inbound/dave/github", bytes.NewReader(validPushBody()))
				req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
				return h, req
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, req := tc.setup(t)
			rr := httptest.NewRecorder()
			h.handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d: %s", rr.Code, rr.Body.String())
			}
			assertUnauthorizedBody(t, rr.Body.String())
		})
	}
}

// TestInboundMethodNotAllowed: GET on the inbound endpoint is 405, not 401 —
// the endpoint exists and is reachable, the method just isn't right.
func TestInboundMethodNotAllowed(t *testing.T) {
	h := buildInboundHarness(t, inboundHarnessConfig{},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte("s")))

	req := httptest.NewRequest(http.MethodGet, "/webhooks/inbound/dave/github", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestInbound_EnvelopeCacheWrite: a matched inbound request writes the
// rendered envelope to the cache under the assigned request ID. The
// cache write is the precondition for Phase 3's replay endpoint.
func TestInbound_EnvelopeCacheWrite(t *testing.T) {
	const secret = "topsecret"
	body := validPushBody()
	sig := signBody([]byte(secret), body, "sha256=")

	cache := inbound.NewMemoryEnvelopeCache()
	h := buildInboundHarness(t, inboundHarnessConfig{envelopeCache: cache},
		inboundAgent("dave", "github", "dave-github-hmac",
			"X-Hub-Signature-256", "sha256="),
		inboundSecret("dave-github-hmac", []byte(secret)),
	)

	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	requestID := rr.Header().Get("X-Inbound-Request-Id")
	if requestID == "" {
		t.Fatalf("missing X-Inbound-Request-Id")
	}

	// Wait for dispatch so we know Decide ran.
	select {
	case <-h.jobDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("queue handler never fired")
	}

	envelope, err := cache.Get(context.Background(), requestID)
	if err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if envelope == "" {
		t.Fatalf("envelope cache empty for request id %q (cache should have been written by the receiver)", requestID)
	}
	if !strings.Contains(envelope, "matty-v/test") {
		t.Errorf("envelope missing repo data; got %q", envelope)
	}
}

// TestInbound_EnvelopeCacheWriteOnRateLimit asserts that even when the
// receiver rate-limits a request (429), the envelope is still written to
// the cache. This is the load-bearing claim that makes "replay a request
// that hit a temporary capacity wall" actually work — Phase 3 review #4
// caught that no test exercises this path end-to-end.
func TestInbound_EnvelopeCacheWriteOnRateLimit(t *testing.T) {
	const secret = "topsecret"
	body := validPushBody()
	sig := signBody([]byte(secret), body, "sha256=")

	cache := inbound.NewMemoryEnvelopeCache()
	agent := inboundAgent("dave", "github", "dave-github-hmac",
		"X-Hub-Signature-256", "sha256=")
	// Rate limit set to 1/min — the second request will be rate-limited.
	agent.Spec.InboundBindings[0].Limits = &kyberv1.AgentInboundLimits{MaxPerMinute: 1}

	h := buildInboundHarness(t, inboundHarnessConfig{envelopeCache: cache},
		agent,
		inboundSecret("dave-github-hmac", []byte(secret)),
	)

	// First request — passes rate limit, dispatches.
	rr := postInbound(t, h, "dave", "github", body, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "push",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Second request — different body (so it's not deduped) but same binding.
	body2 := []byte(`{"repository":{"full_name":"matty-v/test"},"head_commit":{"message":"second"}}`)
	sig2 := signBody([]byte(secret), body2, "sha256=")
	rr2 := postInbound(t, h, "dave", "github", body2, map[string]string{
		"X-Hub-Signature-256": sig2,
		"X-GitHub-Event":      "push",
	})
	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: want 429 (rate-limited), got %d: %s", rr2.Code, rr2.Body.String())
	}

	requestID := rr2.Header().Get("X-Inbound-Request-Id")
	if requestID == "" {
		t.Fatalf("missing X-Inbound-Request-Id on rate-limited response")
	}

	envelope, err := cache.Get(context.Background(), requestID)
	if err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if envelope == "" {
		t.Fatalf("envelope cache empty for rate-limited request %q — replay would fail to load it", requestID)
	}
	if !strings.Contains(envelope, "second") {
		t.Errorf("envelope is the wrong one; got %q", envelope)
	}
}
