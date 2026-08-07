package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/inbound"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// bindingsHarness wires up a fake k8s client and a Server BuildHandler so
// the tests can drive the management API end-to-end.
type bindingsHarness struct {
	handler http.Handler
	k8s     client.Client
	srv     *api.Server
}

func buildBindingsHarness(t *testing.T, objs ...runtime.Object) *bindingsHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()
	srv := &api.Server{
		K8sClient: fakeClient,
		APIKey:    testAPIKey,
		Namespace: "kyber-system",
		PublicURL: "https://kyber-test.example",
	}
	return &bindingsHarness{
		handler: srv.BuildHandler(),
		k8s:     fakeClient,
		srv:     srv,
	}
}

// buildBindingsHarnessWithInterceptor builds a harness whose underlying
// fake client wraps an interceptor.Funcs so a test can fail specific calls
// (e.g. Patch) while still letting earlier Create calls succeed against
// the real fake. Used for the orphan-cleanup test.
func buildBindingsHarnessWithInterceptor(t *testing.T, funcs interceptor.Funcs, objs ...runtime.Object) *bindingsHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&kyberv1.Agent{}).
		WithInterceptorFuncs(funcs).
		Build()
	srv := &api.Server{
		K8sClient: fakeClient,
		APIKey:    testAPIKey,
		Namespace: "kyber-system",
		PublicURL: "https://kyber-test.example",
	}
	return &bindingsHarness{
		handler: srv.BuildHandler(),
		k8s:     fakeClient,
		srv:     srv,
	}
}

// bareAgent returns an Agent CRD with no inbound bindings and no status.
func bareAgent(name string) *kyberv1.Agent {
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
		},
	}
}

// agentWithBindings returns an Agent with two pre-existing bindings, used
// for the GET stats-aggregation test.
func agentWithBindings(name string) *kyberv1.Agent {
	a := bareAgent(name)
	a.Spec.InboundBindings = []kyberv1.AgentInboundBinding{
		{
			Name:            "ci-watch",
			ExistingSecret:  "dave-ci-watch-hmac",
			SignatureHeader: "X-Hub-Signature-256",
			SignaturePrefix: "sha256=",
			EventHeader:     "X-GitHub-Event",
			MatchEvents:     []string{"push"},
			Action:          "investigate",
		},
		{
			Name:            "stripe",
			ExistingSecret:  "dave-stripe-hmac",
			SignatureHeader: "Stripe-Signature",
			Action:          "review the charge",
			Limits:          &kyberv1.AgentInboundLimits{MaxPerMinute: 30},
		},
	}
	return a
}

// stuffStatusRuns is a small helper that writes a synthetic ring buffer to
// status.inboundRuns via the fake client's status subresource. The runs
// must already be populated on the agent passed in.
func stuffStatusRuns(t *testing.T, c client.Client, agentName string, runs []kyberv1.AgentInboundRun) {
	t.Helper()
	a := &kyberv1.Agent{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: agentName, Namespace: "kyber-system"}, a); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	patch := client.MergeFrom(a.DeepCopy())
	a.Status.InboundRuns = runs
	if err := c.Status().Patch(context.Background(), a, patch); err != nil {
		t.Fatalf("patch agent status: %v", err)
	}
}

// makeRun builds a synthetic AgentInboundRun.
func makeRun(binding, outcome, dropReason string, startedAt time.Time) kyberv1.AgentInboundRun {
	t := metav1.NewTime(startedAt)
	return kyberv1.AgentInboundRun{
		BindingName: binding,
		RequestID:   "rid-" + binding + "-" + outcome + "-" + dropReason,
		StartedAt:   &t,
		Outcome:     outcome,
		DropReason:  dropReason,
	}
}

// fetchAgent returns the latest Agent from the harness's fake client.
func (h *bindingsHarness) fetchAgent(t *testing.T, name string) *kyberv1.Agent {
	t.Helper()
	a := &kyberv1.Agent{}
	if err := h.k8s.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "kyber-system"}, a); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	return a
}

// fetchSecret returns the corev1.Secret with the given name, or nil + the
// raw error if missing.
func (h *bindingsHarness) fetchSecret(t *testing.T, name string) (*corev1.Secret, error) {
	t.Helper()
	s := &corev1.Secret{}
	err := h.k8s.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "kyber-system"}, s)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// TestInboundBindings_GET_Empty: no bindings → empty list.
func TestInboundBindings_GET_Empty(t *testing.T) {
	h := buildBindingsHarness(t, bareAgent("dave"))
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/inbound-bindings", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Bindings []map[string]any `json:"bindings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Bindings) != 0 {
		t.Errorf("want empty bindings, got %d", len(got.Bindings))
	}
}

// TestInboundBindings_GET_WithStats: two bindings + a mixture of
// status.inboundRuns; stats are aggregated per binding.
func TestInboundBindings_GET_WithStats(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))

	now := time.Date(2026, 4, 28, 22, 0, 0, 0, time.UTC)
	runs := []kyberv1.AgentInboundRun{
		// ci-watch: 3 dispatched, 2 sig-mismatch, 1 rate-limited.
		makeRun("ci-watch", "dispatched", "", now.Add(-5*time.Minute)),
		makeRun("ci-watch", "dispatched", "", now.Add(-4*time.Minute)),
		makeRun("ci-watch", "dispatched", "", now.Add(-3*time.Minute)),
		makeRun("ci-watch", "dropped", "sig-mismatch", now.Add(-2*time.Minute)),
		makeRun("ci-watch", "dropped", "sig-mismatch", now.Add(-90*time.Second)),
		makeRun("ci-watch", "dropped", "rate-limited", now.Add(-60*time.Second)),
		// stripe: 1 dispatched, 1 dedup.
		makeRun("stripe", "dispatched", "", now.Add(-30*time.Second)),
		makeRun("stripe", "dropped", "dedup", now.Add(-15*time.Second)),
	}
	stuffStatusRuns(t, h.k8s, "dave", runs)

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/inbound-bindings", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var got struct {
		Bindings []struct {
			Name  string `json:"name"`
			URL   string `json:"url"`
			Stats struct {
				LastReceivedAt  string         `json:"lastReceivedAt"`
				TotalDispatched int            `json:"totalDispatched"`
				Dropped         map[string]int `json:"dropped"`
			} `json:"stats"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Bindings) != 2 {
		t.Fatalf("want 2 bindings, got %d", len(got.Bindings))
	}

	byName := map[string]int{}
	for i, b := range got.Bindings {
		byName[b.Name] = i
	}
	ci, ok := byName["ci-watch"]
	if !ok {
		t.Fatalf("ci-watch missing")
	}
	st, ok := byName["stripe"]
	if !ok {
		t.Fatalf("stripe missing")
	}

	// ci-watch assertions.
	if got.Bindings[ci].Stats.TotalDispatched != 3 {
		t.Errorf("ci-watch totalDispatched: got %d, want 3",
			got.Bindings[ci].Stats.TotalDispatched)
	}
	if got.Bindings[ci].Stats.Dropped["sig-mismatch"] != 2 {
		t.Errorf("ci-watch sig-mismatch: got %d, want 2",
			got.Bindings[ci].Stats.Dropped["sig-mismatch"])
	}
	if got.Bindings[ci].Stats.Dropped["rate-limited"] != 1 {
		t.Errorf("ci-watch rate-limited: got %d, want 1",
			got.Bindings[ci].Stats.Dropped["rate-limited"])
	}
	// All 7 drop-reason keys must be present (zero where there were no drops).
	for _, k := range []string{
		"rate-limited", "queue-full", "sig-mismatch", "missing-secret",
		"unmatched-event", "filter-rejected", "dedup",
	} {
		if _, ok := got.Bindings[ci].Stats.Dropped[k]; !ok {
			t.Errorf("ci-watch dropped map missing key %q", k)
		}
	}
	// LastReceivedAt is the most recent StartedAt across all entries.
	if got.Bindings[ci].Stats.LastReceivedAt == "" {
		t.Errorf("ci-watch LastReceivedAt should be set")
	}

	// stripe assertions.
	if got.Bindings[st].Stats.TotalDispatched != 1 {
		t.Errorf("stripe totalDispatched: got %d, want 1",
			got.Bindings[st].Stats.TotalDispatched)
	}
	if got.Bindings[st].Stats.Dropped["dedup"] != 1 {
		t.Errorf("stripe dedup: got %d, want 1",
			got.Bindings[st].Stats.Dropped["dedup"])
	}

	// URL is computed from PublicURL.
	if !strings.HasPrefix(got.Bindings[ci].URL, "https://kyber-test.example/webhooks/inbound/dave/") {
		t.Errorf("ci-watch URL: got %q", got.Bindings[ci].URL)
	}
}

// TestInboundBindings_POST_HappyPath: creates a binding, returns the
// secret + url once, and writes both the K8s Secret and the spec entry.
func TestInboundBindings_POST_HappyPath(t *testing.T) {
	h := buildBindingsHarness(t, bareAgent("dave"))
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/inbound-bindings", map[string]any{
		"name":            "ci-watch",
		"signatureHeader": "X-Hub-Signature-256",
		"signaturePrefix": "sha256=",
		"eventHeader":     "X-GitHub-Event",
		"matchEvents":     []string{"push"},
		"action":          "Please review.",
		"limits":          map[string]any{"maxPerMinute": 10},
	})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Binding struct {
			Name           string `json:"name"`
			ExistingSecret string `json:"existingSecret"`
		} `json:"binding"`
		Secret string `json:"secret"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Binding.Name != "ci-watch" {
		t.Errorf("Name: got %q, want ci-watch", got.Binding.Name)
	}
	if got.Binding.ExistingSecret != "dave-ci-watch-hmac" {
		t.Errorf("ExistingSecret: got %q, want dave-ci-watch-hmac",
			got.Binding.ExistingSecret)
	}
	if got.URL != "https://kyber-test.example/webhooks/inbound/dave/ci-watch" {
		t.Errorf("URL: got %q", got.URL)
	}
	// Secret should be 64 hex chars (32 bytes).
	if len(got.Secret) != 64 {
		t.Errorf("Secret hex length: got %d, want 64", len(got.Secret))
	}
	if _, err := hex.DecodeString(got.Secret); err != nil {
		t.Errorf("Secret not valid hex: %v", err)
	}

	// Spec must contain the new binding.
	a := h.fetchAgent(t, "dave")
	if len(a.Spec.InboundBindings) != 1 {
		t.Fatalf("want 1 binding in spec, got %d", len(a.Spec.InboundBindings))
	}
	if a.Spec.InboundBindings[0].Name != "ci-watch" {
		t.Errorf("spec binding name: got %q", a.Spec.InboundBindings[0].Name)
	}

	// Secret must exist with proper labels and contents.
	sec, err := h.fetchSecret(t, "dave-ci-watch-hmac")
	if err != nil {
		t.Fatalf("fetch secret: %v", err)
	}
	if sec.Labels["app.kubernetes.io/managed-by"] != "kyber-api" {
		t.Errorf("managed-by label missing: %v", sec.Labels)
	}
	if sec.Labels["kyber.io/agent"] != "dave" {
		t.Errorf("kyber.io/agent label: %v", sec.Labels)
	}
	if sec.Labels["kyber.io/binding"] != "ci-watch" {
		t.Errorf("kyber.io/binding label: %v", sec.Labels)
	}
	// 32 random bytes hex-encode to 64 ASCII characters; the K8s Secret
	// stores those 64 characters as bytes (not the raw 32) so the
	// verifier's HMAC key matches the operator-pasted upstream value.
	if got, want := len(sec.Data["webhook-secret"]), 64; got != want {
		t.Errorf("webhook-secret bytes (hex-encoded): got %d, want %d", got, want)
	}
	if string(sec.Data["webhook-secret"]) != got.Secret {
		t.Errorf("Secret data should equal the response hex string verbatim")
	}

	// End-to-end interop check: signing a body with the response's hex
	// string (= what an operator pastes into GitHub's webhook config) must
	// verify against the K8s Secret the receiver will load. If this fails,
	// every real-world inbound request would 401 — see kyber#208 review #1.
	body := []byte(`{"test":"interop"}`)
	sigBytes := hmacSHA256(sec.Data["webhook-secret"], body)
	if err := inbound.Verify(sec.Data["webhook-secret"], body,
		"sha256="+hex.EncodeToString(sigBytes), "sha256="); err != nil {
		t.Fatalf("interop check: response hex must HMAC-verify against stored Secret: %v", err)
	}
}

// hmacSHA256 returns HMAC-SHA256(body) keyed by the given key bytes.
// Test helper — mirrors what GitHub does when computing X-Hub-Signature-256.
func hmacSHA256(key, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return mac.Sum(nil)
}

// TestInboundBindings_POST_NameCollision: 409 when the spec already has
// a binding with this name.
func TestInboundBindings_POST_NameCollision(t *testing.T) {
	a := bareAgent("dave")
	a.Spec.InboundBindings = []kyberv1.AgentInboundBinding{
		{
			Name:            "ci-watch",
			ExistingSecret:  "dave-ci-watch-hmac",
			SignatureHeader: "X-Hub-Signature-256",
			Action:          "x",
		},
	}
	h := buildBindingsHarness(t, a)
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/inbound-bindings", map[string]any{
		"name":            "ci-watch",
		"signatureHeader": "X-Hub-Signature-256",
		"action":          "again",
	})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("want 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestInboundBindings_POST_MissingAction: 400 with field error.
func TestInboundBindings_POST_MissingAction(t *testing.T) {
	h := buildBindingsHarness(t, bareAgent("dave"))
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/inbound-bindings", map[string]any{
		"name":            "ci-watch",
		"signatureHeader": "X-Hub-Signature-256",
		"action":          "",
	})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"field":"action"`) {
		t.Errorf("expected field=action, got %s", body)
	}
}

// TestInboundBindings_POST_MissingSignatureHeader: 400 with field error.
func TestInboundBindings_POST_MissingSignatureHeader(t *testing.T) {
	h := buildBindingsHarness(t, bareAgent("dave"))
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/inbound-bindings", map[string]any{
		"name":   "ci-watch",
		"action": "x",
	})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"field":"signatureHeader"`) {
		t.Errorf("expected field=signatureHeader, got %s", rr.Body.String())
	}
}

// TestInboundBindings_POST_BadName: regex rejects "Bad-Name".
func TestInboundBindings_POST_BadName(t *testing.T) {
	h := buildBindingsHarness(t, bareAgent("dave"))
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/inbound-bindings", map[string]any{
		"name":            "Bad-Name",
		"signatureHeader": "X-Hub-Signature-256",
		"action":          "x",
	})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"field":"name"`) {
		t.Errorf("expected field=name, got %s", rr.Body.String())
	}
}

// TestInboundBindings_POST_OrphanSecretCleanup: when the Patch on the
// Agent fails AFTER the Secret is created, the orphan Secret is deleted.
func TestInboundBindings_POST_OrphanSecretCleanup(t *testing.T) {
	patchFails := errors.New("synthetic: patch failed")
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			// Only fail Agent patches — the Secret patch path (used by
			// rotate-secret tests in the same package) must still work
			// elsewhere. Here we know we're only doing an Agent patch.
			if _, ok := obj.(*kyberv1.Agent); ok {
				return patchFails
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	h := buildBindingsHarnessWithInterceptor(t, funcs, bareAgent("dave"))

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/dave/inbound-bindings", map[string]any{
		"name":            "ci-watch",
		"signatureHeader": "X-Hub-Signature-256",
		"action":          "x",
	})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rr.Code, rr.Body.String())
	}
	// Allow the detached cleanup goroutine a moment if needed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := h.fetchSecret(t, "dave-ci-watch-hmac")
		if err != nil {
			// Got an error → secret is gone.
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("orphan secret was not cleaned up")
}

// TestInboundBindings_DELETE_HappyPath: removes binding from spec, deletes
// the Secret, returns 204.
func TestInboundBindings_DELETE_HappyPath(t *testing.T) {
	a := bareAgent("dave")
	a.Spec.InboundBindings = []kyberv1.AgentInboundBinding{
		{
			Name:            "ci-watch",
			ExistingSecret:  "dave-ci-watch-hmac",
			SignatureHeader: "X-Hub-Signature-256",
			Action:          "x",
		},
	}
	h := buildBindingsHarness(t, a, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dave-ci-watch-hmac", Namespace: "kyber-system",
		},
		Data: map[string][]byte{"webhook-secret": []byte("rawsecret")},
	})
	req := authedRequest(t, http.MethodDelete, "/api/v1/agents/dave/inbound-bindings/ci-watch", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}
	// Spec should no longer have the binding.
	got := h.fetchAgent(t, "dave")
	if len(got.Spec.InboundBindings) != 0 {
		t.Errorf("want spec empty, got %d", len(got.Spec.InboundBindings))
	}
	// Secret should be gone.
	if _, err := h.fetchSecret(t, "dave-ci-watch-hmac"); err == nil {
		t.Errorf("secret should have been deleted")
	}
}

// TestInboundBindings_DELETE_SecretAlreadyMissing: secret already gone,
// DELETE still 204s and removes the spec entry.
func TestInboundBindings_DELETE_SecretAlreadyMissing(t *testing.T) {
	a := bareAgent("dave")
	a.Spec.InboundBindings = []kyberv1.AgentInboundBinding{
		{
			Name:            "ci-watch",
			ExistingSecret:  "dave-ci-watch-hmac",
			SignatureHeader: "X-Hub-Signature-256",
			Action:          "x",
		},
	}
	// No secret seeded.
	h := buildBindingsHarness(t, a)
	req := authedRequest(t, http.MethodDelete, "/api/v1/agents/dave/inbound-bindings/ci-watch", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}
	got := h.fetchAgent(t, "dave")
	if len(got.Spec.InboundBindings) != 0 {
		t.Errorf("want spec empty, got %d", len(got.Spec.InboundBindings))
	}
}

// TestInboundBindings_DELETE_UnknownBinding: 404 with body
// {"error":{"message":"binding not found"}}.
func TestInboundBindings_DELETE_UnknownBinding(t *testing.T) {
	h := buildBindingsHarness(t, bareAgent("dave"))
	req := authedRequest(t, http.MethodDelete, "/api/v1/agents/dave/inbound-bindings/ghost", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "binding not found") {
		t.Errorf("expected 'binding not found', got %s", rr.Body.String())
	}
}

// TestInboundBindings_Rotate_HappyPath: new bytes in webhook-secret, old
// preserved in webhook-secret-prev, rotated-at timestamp present and parses.
func TestInboundBindings_Rotate_HappyPath(t *testing.T) {
	a := bareAgent("dave")
	a.Spec.InboundBindings = []kyberv1.AgentInboundBinding{
		{
			Name:            "ci-watch",
			ExistingSecret:  "dave-ci-watch-hmac",
			SignatureHeader: "X-Hub-Signature-256",
			Action:          "x",
		},
	}
	originalBytes := []byte("originalrawsecret-bytes-32-bytes")
	if len(originalBytes) != 32 {
		// Adjust to be exactly 32 bytes if message above doesn't compute.
		originalBytes = []byte("0123456789abcdef0123456789abcdef")
	}
	h := buildBindingsHarness(t, a, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dave-ci-watch-hmac", Namespace: "kyber-system",
		},
		Data: map[string][]byte{"webhook-secret": originalBytes},
	})

	req := authedRequest(t, http.MethodPost,
		"/api/v1/agents/dave/inbound-bindings/ci-watch/rotate-secret", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Secret    string `json:"secret"`
		RotatedAt string `json:"rotatedAt"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Secret) != 64 {
		t.Errorf("Secret hex length: got %d, want 64", len(got.Secret))
	}
	if got.RotatedAt == "" {
		t.Errorf("RotatedAt empty")
	}
	if _, err := time.Parse(time.RFC3339, got.RotatedAt); err != nil {
		t.Errorf("RotatedAt parse: %v", err)
	}

	sec, err := h.fetchSecret(t, "dave-ci-watch-hmac")
	if err != nil {
		t.Fatalf("fetch secret: %v", err)
	}
	// New hex string lives in webhook-secret (stored as ASCII bytes —
	// same encoding contract as create, so the operator-pasted value and
	// the verifier's HMAC key agree byte-for-byte).
	if string(sec.Data["webhook-secret"]) != got.Secret {
		t.Errorf("webhook-secret was not updated to new hex string")
	}
	if len(sec.Data["webhook-secret"]) != 64 {
		t.Errorf("webhook-secret length: got %d, want 64", len(sec.Data["webhook-secret"]))
	}
	// Old bytes preserved as webhook-secret-prev.
	if string(sec.Data["webhook-secret-prev"]) != string(originalBytes) {
		t.Errorf("webhook-secret-prev: got %q, want %q",
			sec.Data["webhook-secret-prev"], originalBytes)
	}
	if len(sec.Data["webhook-secret-prev-rotated-at"]) == 0 {
		t.Errorf("webhook-secret-prev-rotated-at is empty")
	}
	if _, err := time.Parse(time.RFC3339, string(sec.Data["webhook-secret-prev-rotated-at"])); err != nil {
		t.Errorf("rotated-at not RFC3339: %v", err)
	}
}

// TestInboundBindings_Rotate_UnknownBinding: 404.
func TestInboundBindings_Rotate_UnknownBinding(t *testing.T) {
	h := buildBindingsHarness(t, bareAgent("dave"))
	req := authedRequest(t, http.MethodPost,
		"/api/v1/agents/dave/inbound-bindings/ghost/rotate-secret", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestInboundBindings_MethodPermutations: PUT/PATCH on the collection are
// 405; bad regex names are 400.
func TestInboundBindings_MethodPermutations(t *testing.T) {
	h := buildBindingsHarness(t, bareAgent("dave"))
	cases := []struct {
		method   string
		path     string
		wantCode int
	}{
		{http.MethodPut, "/api/v1/agents/dave/inbound-bindings", http.StatusMethodNotAllowed},
		{http.MethodPatch, "/api/v1/agents/dave/inbound-bindings", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/agents/dave/inbound-bindings/Bad-Name", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := authedRequest(t, tc.method, tc.path, nil)
			rr := httptest.NewRecorder()
			h.handler.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Errorf("want %d, got %d: %s", tc.wantCode, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestInboundBindings_GET_AgentNotFound: 404 when the agent doesn't exist.
func TestInboundBindings_GET_AgentNotFound(t *testing.T) {
	h := buildBindingsHarness(t)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/ghost/inbound-bindings", nil)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// ---- PATCH /api/v1/agents/{name}/inbound-bindings/{binding} (#222) ----

// TestInboundBindings_PATCH_HappyPath: valid edit replaces the matched
// binding's editable fields and returns the updated spec.
func TestInboundBindings_PATCH_HappyPath(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))

	body := map[string]any{
		"action":      "investigate and ping Matt on Telegram",
		"matchEvents": []string{"push", "pull_request"},
		"limits":      map[string]any{"maxPerMinute": 60},
	}
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave/inbound-bindings/ci-watch", body)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Name        string   `json:"name"`
		Action      string   `json:"action"`
		MatchEvents []string `json:"matchEvents"`
		Limits      struct {
			MaxPerMinute int `json:"maxPerMinute"`
		} `json:"limits"`
		ExistingSecret string `json:"existingSecret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != "ci-watch" {
		t.Errorf("name: got %q, want ci-watch", got.Name)
	}
	if got.Action != "investigate and ping Matt on Telegram" {
		t.Errorf("action: got %q", got.Action)
	}
	if !reflect.DeepEqual(got.MatchEvents, []string{"push", "pull_request"}) {
		t.Errorf("matchEvents: got %v", got.MatchEvents)
	}
	if got.Limits.MaxPerMinute != 60 {
		t.Errorf("limits.maxPerMinute: got %d, want 60", got.Limits.MaxPerMinute)
	}
	// existingSecret stays as-is — operator's pasted upstream URL still works.
	if got.ExistingSecret != "dave-ci-watch-hmac" {
		t.Errorf("existingSecret should be unchanged, got %q", got.ExistingSecret)
	}

	// Confirm the on-disk Agent reflects the patch.
	updated := h.fetchAgent(t, "dave")
	var found *kyberv1.AgentInboundBinding
	for i, b := range updated.Spec.InboundBindings {
		if b.Name == "ci-watch" {
			found = &updated.Spec.InboundBindings[i]
			break
		}
	}
	if found == nil {
		t.Fatal("ci-watch missing from updated agent")
	}
	if found.Action != "investigate and ping Matt on Telegram" {
		t.Errorf("on-disk action: got %q", found.Action)
	}
	if found.ExistingSecret != "dave-ci-watch-hmac" {
		t.Errorf("on-disk existingSecret: got %q", found.ExistingSecret)
	}
}

// TestInboundBindings_PATCH_PreservesUnreferencedFields: a partial body
// only touches the fields it sends.
func TestInboundBindings_PATCH_PreservesUnreferencedFields(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))

	body := map[string]any{"action": "new action only"}
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave/inbound-bindings/ci-watch", body)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	updated := h.fetchAgent(t, "dave")
	for _, b := range updated.Spec.InboundBindings {
		if b.Name != "ci-watch" {
			continue
		}
		// Touched.
		if b.Action != "new action only" {
			t.Errorf("action: got %q", b.Action)
		}
		// Untouched.
		if b.SignatureHeader != "X-Hub-Signature-256" {
			t.Errorf("signatureHeader: got %q", b.SignatureHeader)
		}
		if b.EventHeader != "X-GitHub-Event" {
			t.Errorf("eventHeader: got %q", b.EventHeader)
		}
		if !reflect.DeepEqual(b.MatchEvents, []string{"push"}) {
			t.Errorf("matchEvents: got %v", b.MatchEvents)
		}
	}
}

// TestInboundBindings_PATCH_RejectsName_ReadOnly: name in the body is rejected
// with a 400 + field=name. Operators must DELETE + POST to rename.
func TestInboundBindings_PATCH_RejectsName_ReadOnly(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave/inbound-bindings/ci-watch",
		map[string]any{"name": "renamed"})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Error struct {
			Code  string `json:"code"`
			Field string `json:"field"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Error.Field != "name" {
		t.Errorf("error.field: got %q, want name", got.Error.Field)
	}
}

// TestInboundBindings_PATCH_RejectsExistingSecret_ReadOnly: existingSecret in
// the body is rejected with a 400 + field=existingSecret.
func TestInboundBindings_PATCH_RejectsExistingSecret_ReadOnly(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave/inbound-bindings/ci-watch",
		map[string]any{"existingSecret": "dave-ci-watch-hmac-v2"})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Error struct {
			Field string `json:"field"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Error.Field != "existingSecret" {
		t.Errorf("error.field: got %q, want existingSecret", got.Error.Field)
	}
}

// TestInboundBindings_PATCH_BindingNotFound: 404 when the binding doesn't
// exist on the agent (vs the agent not existing at all, which is also 404).
func TestInboundBindings_PATCH_BindingNotFound(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave/inbound-bindings/missing",
		map[string]any{"action": "x"})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestInboundBindings_PATCH_AgentNotFound: 404 when the agent doesn't exist.
func TestInboundBindings_PATCH_AgentNotFound(t *testing.T) {
	h := buildBindingsHarness(t)
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/ghost/inbound-bindings/anything",
		map[string]any{"action": "x"})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestInboundBindings_PATCH_RejectsEmptyAction: an explicit empty action
// is a validation error so an operator can't accidentally clear it.
func TestInboundBindings_PATCH_RejectsEmptyAction(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave/inbound-bindings/ci-watch",
		map[string]any{"action": ""})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestInboundBindings_PATCH_PreservesInboundRuns: status.inboundRuns are
// not touched by the spec patch — the binding's name doesn't change so its
// per-binding ring buffer stays attached.
func TestInboundBindings_PATCH_PreservesInboundRuns(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))
	now := time.Date(2026, 5, 3, 14, 0, 0, 0, time.UTC)
	runs := []kyberv1.AgentInboundRun{
		makeRun("ci-watch", "dispatched", "", now.Add(-2*time.Minute)),
		makeRun("ci-watch", "dispatched", "", now.Add(-time.Minute)),
		makeRun("stripe", "dispatched", "", now.Add(-30*time.Second)),
	}
	stuffStatusRuns(t, h.k8s, "dave", runs)

	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave/inbound-bindings/ci-watch",
		map[string]any{"action": "updated"})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	updated := h.fetchAgent(t, "dave")
	if len(updated.Status.InboundRuns) != 3 {
		t.Errorf("status.inboundRuns lost entries: got %d, want 3", len(updated.Status.InboundRuns))
	}
}

// TestInboundBindings_PATCH_UnknownField: an unknown JSON field is a
// validation error rather than a silent no-op — keeps the PWA honest about
// what's editable.
func TestInboundBindings_PATCH_UnknownField(t *testing.T) {
	h := buildBindingsHarness(t, agentWithBindings("dave"))
	req := authedRequest(t, http.MethodPatch, "/api/v1/agents/dave/inbound-bindings/ci-watch",
		map[string]any{"unknownField": "value"})
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400 on unknown field, got %d: %s", rr.Code, rr.Body.String())
	}
}
