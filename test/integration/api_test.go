//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// sendWebhook sends a Telegram update JSON to the webhook endpoint and returns the response.
func sendWebhook(t *testing.T, h http.Handler, agentName, text, from string) *httptest.ResponseRecorder {
	t.Helper()
	update := map[string]interface{}{
		"update_id": 12345,
		"message": map[string]interface{}{
			"message_id": 1,
			"text":       text,
			"from": map[string]interface{}{
				"id":         42,
				"username":   from,
				"first_name": "Matt",
			},
			"chat": map[string]interface{}{
				"id": 42,
			},
		},
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(update); err != nil {
		t.Fatalf("encoding webhook body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/"+agentName, &buf)
	req.Header.Set("Content-Type", "application/json")
	// kyber#564 fail-closed: the webhook routes now always validate the secret,
	// so send the matching header the test server is configured with — otherwise
	// the request takes the drop-with-200 reject path and nothing is buffered.
	req.Header.Set(api.TelegramWebhookSecretHeader, testWebhookSecret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// sampleAgentObj returns a pre-built Agent CRD for integration tests.
func sampleAgentObj(name string, phase kyberv1.AgentPhase) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
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
			Secrets:      kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			DesiredPhase: kyberv1.AgentPhaseRunning,
		},
		Status: kyberv1.AgentStatus{
			Phase: phase,
		},
	}
}

// TestAPI_WebhookBuffersMessageInRedis verifies the webhook handler pushes messages
// to the real Redis buffer and that a subsequent Drain retrieves them.
func TestAPI_WebhookBuffersMessageInRedis(t *testing.T) {
	agentName := fmt.Sprintf("webhook-test-%d", time.Now().UnixNano())
	cleanRedisKey(t, "wake:"+agentName)

	// Use real Redis buffer.
	redisBuf := messagebuffer.NewRedisBuffer(sharedRDB)
	agent := sampleAgentObj(agentName, kyberv1.AgentPhaseRunning)
	srv := newTestAPIServer(t, redisBuf, agent)
	h := srv.BuildHandler()

	// Send a webhook message.
	rr := sendWebhook(t, h, agentName, "hello from integration test", "matt")
	if rr.Code != http.StatusOK {
		t.Fatalf("webhook response: got %d, want 200", rr.Code)
	}

	// The buffer should now have the message.
	msgs, err := redisBuf.Drain(context.Background(), agentName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 buffered message, got %d", len(msgs))
	}
	if msgs[0].Text != "hello from integration test" {
		t.Errorf("message text: got %q, want %q", msgs[0].Text, "hello from integration test")
	}
	if msgs[0].Source != "telegram" {
		t.Errorf("message source: got %q, want %q", msgs[0].Source, "telegram")
	}
}

// TestAPI_WebhookSurvivesRestart verifies that messages buffered by one API instance
// are visible to a fresh API instance reading from the same Redis — simulating a restart.
func TestAPI_WebhookSurvivesRestart(t *testing.T) {
	agentName := fmt.Sprintf("restart-test-%d", time.Now().UnixNano())
	cleanRedisKey(t, "wake:"+agentName)

	// First API "instance".
	redisBuf1 := messagebuffer.NewRedisBuffer(sharedRDB)
	agent := sampleAgentObj(agentName, kyberv1.AgentPhaseRunning)
	srv1 := newTestAPIServer(t, redisBuf1, agent)
	h1 := srv1.BuildHandler()

	// Send messages via the first instance.
	texts := []string{"msg-one", "msg-two", "msg-three"}
	for _, text := range texts {
		rr := sendWebhook(t, h1, agentName, text, "matt")
		if rr.Code != http.StatusOK {
			t.Fatalf("webhook via srv1: got %d, want 200", rr.Code)
		}
	}

	// Simulate restart: new Redis client (fresh connection), same Redis data.
	redisBuf2 := messagebuffer.NewRedisBuffer(sharedRDB)

	// Drain via the second instance — messages must still be there.
	msgs, err := redisBuf2.Drain(context.Background(), agentName)
	if err != nil {
		t.Fatalf("Drain via second instance: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("second instance: got %d messages, want 3", len(msgs))
	}
	for i, msg := range msgs {
		if msg.Text != texts[i] {
			t.Errorf("msgs[%d].Text = %q, want %q", i, msg.Text, texts[i])
		}
	}
}

// TestAPI_WebhookMultipleMessages verifies multiple sequential webhooks all get buffered.
func TestAPI_WebhookMultipleMessages(t *testing.T) {
	agentName := fmt.Sprintf("multi-msg-%d", time.Now().UnixNano())
	cleanRedisKey(t, "wake:"+agentName)

	redisBuf := messagebuffer.NewRedisBuffer(sharedRDB)
	agent := sampleAgentObj(agentName, kyberv1.AgentPhaseSuspended)
	srv := newTestAPIServer(t, redisBuf, agent)
	h := srv.BuildHandler()

	count := 5
	for i := 0; i < count; i++ {
		text := fmt.Sprintf("message-%d", i)
		rr := sendWebhook(t, h, agentName, text, "matt")
		if rr.Code != http.StatusOK {
			t.Fatalf("webhook %d: got %d, want 200", i, rr.Code)
		}
	}

	msgs, err := redisBuf.Drain(context.Background(), agentName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(msgs) != count {
		t.Fatalf("got %d messages, want %d", len(msgs), count)
	}
}

// TestAPI_WebhookUnknownAgentStillReturns200 verifies that webhooks for unknown
// agents always return 200 (so Telegram doesn't retry).
func TestAPI_WebhookUnknownAgentStillReturns200(t *testing.T) {
	agentName := fmt.Sprintf("unknown-%d", time.Now().UnixNano())
	cleanRedisKey(t, "wake:"+agentName)

	redisBuf := messagebuffer.NewRedisBuffer(sharedRDB)
	// Don't add any agent objects — server has empty k8s state.
	srv := newTestAPIServer(t, redisBuf)
	h := srv.BuildHandler()

	rr := sendWebhook(t, h, agentName, "orphan message", "matt")
	if rr.Code != http.StatusOK {
		t.Errorf("unknown agent webhook: got %d, want 200", rr.Code)
	}
}
