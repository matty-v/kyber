package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestHermesCreationUsesGenericRuntimeCredential(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(defaultMachine()).Build()
	server := &api.Server{
		K8sClient:     client,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"hermes": true},
		RuntimeImages: map[string]string{"hermes": "test.invalid/hermes:pinned"},
	}
	body := `{"name":"hermes-preview","machine":"worker-1","runtime":"hermes","model":"anthropic/claude-sonnet-4.6","secrets":{"runtimeAuth":{"openrouterApiKey":"test-openrouter-value"},"telegramEnabled":true,"telegramBotToken":"123:abc","telegramAllowedUserIds":["1000000001"]}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	server.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "test-openrouter-value") {
		t.Fatal("credential echoed in response")
	}
	agent := &kyberv1.Agent{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "hermes-preview"}, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeAPIKey {
		t.Fatalf("default auth type = %q, want api-key", agent.Spec.Secrets.AuthType)
	}
	secret := &corev1.Secret{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "hermes-preview-openrouter"}, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["token"]) != "test-openrouter-value" {
		t.Fatal("OpenRouter credential was not persisted through the runtime strategy")
	}
	telegram := &corev1.Secret{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "hermes-preview-telegram"}, telegram); err != nil {
		t.Fatal(err)
	}
	if string(telegram.Data["token"]) != "123:abc" {
		t.Fatal("Telegram credential was not persisted for Hermes API-key auth")
	}
}

func TestHermesCreationRejectsUnpinnedImage(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(defaultMachine()).Build()
	server := &api.Server{
		K8sClient:     client,
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"hermes": true},
		RuntimeImages: map[string]string{"hermes": ""},
	}
	body := `{"name":"hermes-preview","machine":"worker-1","runtime":"hermes","secrets":{"authType":"api-key","runtimeAuth":{"openrouterApiKey":"test-openrouter-value"}}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	server.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "image.hermes.tag") {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
}
