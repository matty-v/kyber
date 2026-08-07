package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
	"github.com/matty-v/kyber/pkg/oauth/mockserver"
)

// pkceChallenge computes the S256 PKCE challenge for the given verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestReauthorize_ExchangesAndPatchesSecret(t *testing.T) {
	mock := mockserver.New()
	mockSrv := httptest.NewServer(mock)
	defer mockSrv.Close()

	// IssueCode takes the PKCE challenge (hash of verifier), not the verifier.
	verifier := "test-verifier-value"
	code := mock.IssueCode(pkceChallenge(verifier))

	// Build the agent CRD in NeedsAuth + its oauth secret.
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "needy", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "testvm",
			Runtime: "claude-code",
		},
		Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseNeedsAuth},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "needy-oauth", Namespace: "kyber-system"},
		Data:       map[string][]byte{"refresh_token": []byte("old-token")},
	}

	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(defaultMachine(), agent, secret).
		WithStatusSubresource(agent).
		Build()
	s := &api.Server{
		K8sClient:         fakeClient,
		MessageBuffer:     messagebuffer.NewMemoryBuffer(),
		APIKey:            testAPIKey,
		Namespace:         "kyber-system",
		ValidRuntimes:     map[string]bool{"claude-code": true},
		AnthropicTokenURL: mockSrv.URL + "/v1/oauth/token",
	}
	handler := s.BuildHandler()

	// POST the reauthorize request.
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/needy/oauth", map[string]string{
		"oauthCode":    code,
		"pkceVerifier": verifier,
		"state":        "some-state",
	})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify the secret was updated.
	updatedSecret := &corev1.Secret{}
	_ = fakeClient.Get(t.Context(), types.NamespacedName{Name: "needy-oauth", Namespace: "kyber-system"}, updatedSecret)
	if string(updatedSecret.Data["refresh_token"]) == "old-token" {
		t.Error("refresh_token was not updated")
	}
	if len(updatedSecret.Data["access_token"]) == 0 {
		t.Error("access_token is empty after reauthorize")
	}

	// Verify the agent's desiredPhase was set to Running.
	updatedAgent := &kyberv1.Agent{}
	_ = fakeClient.Get(t.Context(), types.NamespacedName{Name: "needy", Namespace: "kyber-system"}, updatedAgent)
	if updatedAgent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Errorf("desiredPhase=%q, want Running", updatedAgent.Spec.DesiredPhase)
	}
}

func TestReauthorize_AgentNotFound(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(defaultMachine()).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}
	handler := s.BuildHandler()

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/ghost/oauth", map[string]string{
		"oauthCode":    "x",
		"pkceVerifier": "y",
		"state":        "z",
	})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReauthorize_MissingFields(t *testing.T) {
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(defaultMachine()).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{"claude-code": true},
	}
	handler := s.BuildHandler()

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/ghost/oauth", map[string]string{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}
