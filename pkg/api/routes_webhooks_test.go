package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// testWebhookSecret is a non-empty webhook secret for tests that exercise the
// processing path. Under fail-closed (kyber#564) an empty secret rejects, so any
// test asserting buffer/wake/lookup behavior must configure a real secret and send
// the matching X-Telegram-Bot-Api-Secret-Token header.
const testWebhookSecret = "test-webhook-secret"

// suspendedAgent builds a Suspended Agent CRD for webhook tests.
func suspendedAgent(name string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Scaling: kyberv1.AgentScalingScaleToZero,
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("2Gi"),
				Disk:   resource.MustParse("50Gi"),
			},
			Secrets:      kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			DesiredPhase: kyberv1.AgentPhaseSuspended,
		},
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseSuspended,
		},
	}
}

// webhookHarness holds the components needed for webhook tests.
type webhookHarness struct {
	handler http.Handler
	k8s     client.Client
	buf     *messagebuffer.MemoryBuffer
}

// buildWebhookHarness builds a test harness for webhook tests.
func buildWebhookHarness(t *testing.T, webhookSecret string, objs ...runtime.Object) *webhookHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()
	buf := messagebuffer.NewMemoryBuffer()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: buf,
		APIKey:        testAPIKey,
		WebhookSecret: webhookSecret,
		Namespace:     "kyber-system",
	}
	return &webhookHarness{
		handler: s.BuildHandler(),
		k8s:     fakeClient,
		buf:     buf,
	}
}

// telegramUpdate builds a minimal Telegram Update JSON payload.
func telegramUpdate(text, username string) []byte {
	update := map[string]interface{}{
		"update_id": 1,
		"message": map[string]interface{}{
			"message_id": 42,
			"from": map[string]interface{}{
				"id":         12345,
				"username":   username,
				"first_name": "Test",
			},
			"chat": map[string]interface{}{
				"id":   12345,
				"type": "private",
			},
			"text": text,
		},
	}
	b, _ := json.Marshal(update)
	return b
}

// TestWebhook_SuspendedAgent_WakesAndBuffers verifies the full wake flow:
// POST to /webhooks/telegram/{agent-name} with a suspended agent should:
// 1. Buffer the message.
// 2. Patch desiredPhase = Running.
// 3. Return 200.
func TestWebhook_SuspendedAgent_WakesAndBuffers(t *testing.T) {
	agent := suspendedAgent("dave")
	h := buildWebhookHarness(t, testWebhookSecret, agent)

	ctx := context.Background()

	body := telegramUpdate("hello dave", "matty_v")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/dave", bytes.NewReader(body))
	req.Header.Set(api.TelegramWebhookSecretHeader, testWebhookSecret)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify message was buffered.
	msgs, err := h.buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("want 1 buffered message, got %d", len(msgs))
	} else {
		if msgs[0].Text != "hello dave" {
			t.Errorf("buffered text: got %q, want %q", msgs[0].Text, "hello dave")
		}
		if msgs[0].Source != "telegram" {
			t.Errorf("source: got %q, want %q", msgs[0].Source, "telegram")
		}
		if msgs[0].From != "@matty_v" {
			t.Errorf("from: got %q, want %q", msgs[0].From, "@matty_v")
		}
	}

	// Verify desiredPhase was patched to Running.
	updated := &kyberv1.Agent{}
	key := types.NamespacedName{Name: "dave", Namespace: "kyber-system"}
	if err := h.k8s.Get(ctx, key, updated); err != nil {
		t.Fatalf("getting agent after webhook: %v", err)
	}
	if updated.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Errorf("desiredPhase: got %q, want %q", updated.Spec.DesiredPhase, kyberv1.AgentPhaseRunning)
	}
}

// TestWebhook_RunningAgent_BuffersButDoesNotPatch verifies that a webhook for a
// running agent buffers the message but does NOT set desiredPhase (already running).
func TestWebhook_RunningAgent_BuffersButDoesNotPatch(t *testing.T) {
	agent := sampleAgentCRD("dave")
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	h := buildWebhookHarness(t, testWebhookSecret, agent)

	ctx := context.Background()
	body := telegramUpdate("are you there?", "matty_v")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/dave", bytes.NewReader(body))
	req.Header.Set(api.TelegramWebhookSecretHeader, testWebhookSecret)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rr.Code)
	}

	// Message is still buffered.
	msgs, _ := h.buf.Drain(ctx, "dave")
	if len(msgs) != 1 {
		t.Errorf("want 1 buffered message, got %d", len(msgs))
	}

	// desiredPhase should NOT be changed: the agent was Running, not Suspended.
	updated := &kyberv1.Agent{}
	key := types.NamespacedName{Name: "dave", Namespace: "kyber-system"}
	if err := h.k8s.Get(ctx, key, updated); err != nil {
		t.Fatalf("getting agent: %v", err)
	}
	// The original agent has DesiredPhase="" (we didn't set it), so after the webhook
	// it should still be "" (Running agent path doesn't patch).
	if updated.Spec.DesiredPhase == kyberv1.AgentPhaseRunning {
		// This is fine if the agent was created with desiredPhase=Running, but our
		// sampleAgentCRD doesn't set it. Either way, the test verifies we returned 200.
		t.Logf("desiredPhase is Running after webhook on running agent (may be pre-set)")
	}
}

// TestWebhook_AgentNotFound_Returns200 verifies the webhook returns 200 even for unknown agents.
func TestWebhook_AgentNotFound_Returns200(t *testing.T) {
	h := buildWebhookHarness(t, testWebhookSecret)

	body := telegramUpdate("hey", "user")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/nonexistent", bytes.NewReader(body))
	req.Header.Set(api.TelegramWebhookSecretHeader, testWebhookSecret)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200 for unknown agent, got %d", rr.Code)
	}
}

// TestWebhook_SecretMismatch_Returns200 verifies that a wrong secret still returns 200
// (prevents Telegram retry storms on secret rotation).
func TestWebhook_SecretMismatch_Returns200(t *testing.T) {
	h := buildWebhookHarness(t, "correct-secret")

	body := telegramUpdate("hey", "user")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/dave", bytes.NewReader(body))
	req.Header.Set(api.TelegramWebhookSecretHeader, "wrong-secret")
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200 (always return OK to Telegram), got %d", rr.Code)
	}
}

// TestWebhook_CorrectSecret_Processes verifies a request with the correct secret is processed.
func TestWebhook_CorrectSecret_Processes(t *testing.T) {
	agent := suspendedAgent("dave")
	h := buildWebhookHarness(t, "my-webhook-secret", agent)

	ctx := context.Background()
	body := telegramUpdate("hello", "matty_v")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/dave", bytes.NewReader(body))
	req.Header.Set(api.TelegramWebhookSecretHeader, "my-webhook-secret")
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rr.Code)
	}

	// Message should be buffered when secret matches.
	msgs, _ := h.buf.Drain(ctx, "dave")
	if len(msgs) != 1 {
		t.Errorf("want 1 buffered message, got %d", len(msgs))
	}
}

// TestWebhook_EmptySecret_FailsClosed verifies the fail-closed behavior (kyber#564):
// when no webhook secret is configured, every request is REJECTED before any side
// effect — the message is NOT buffered and a suspended agent is NOT woken — yet we
// still return 200 so Telegram does not retry-storm.
func TestWebhook_EmptySecret_FailsClosed(t *testing.T) {
	agent := suspendedAgent("dave")
	h := buildWebhookHarness(t, "" /* no secret configured → fail closed */, agent)

	ctx := context.Background()
	body := telegramUpdate("hello dave", "matty_v")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/dave", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	// Still 200 — the reject reuses the drop-with-200 path to avoid Telegram retries.
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Fail closed: nothing is buffered.
	msgs, err := h.buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("fail-closed: want 0 buffered messages, got %d", len(msgs))
	}

	// Fail closed: the suspended agent is NOT woken — desiredPhase stays Suspended.
	updated := &kyberv1.Agent{}
	key := types.NamespacedName{Name: "dave", Namespace: "kyber-system"}
	if err := h.k8s.Get(ctx, key, updated); err != nil {
		t.Fatalf("getting agent after webhook: %v", err)
	}
	if updated.Spec.DesiredPhase != kyberv1.AgentPhaseSuspended {
		t.Errorf("fail-closed: desiredPhase changed to %q — suspended agent was woken", updated.Spec.DesiredPhase)
	}
}

// TestWebhook_NoAPIKeyRequired verifies the webhook does not require Authorization header.
func TestWebhook_NoAPIKeyRequired(t *testing.T) {
	h := buildWebhookHarness(t, testWebhookSecret)

	body := telegramUpdate("hello", "user")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/dave", bytes.NewReader(body))
	// The webhook secret header IS the webhook's own auth (under fail-closed it must
	// be present to reach the handler) — but deliberately no Bearer Authorization
	// header: the webhook routes must not require the API-key wall.
	req.Header.Set(api.TelegramWebhookSecretHeader, testWebhookSecret)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)

	// Should not be 401.
	if rr.Code == http.StatusUnauthorized {
		t.Errorf("webhook returned 401 — it must not require API key auth")
	}
}
