package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

func TestCodexDeviceAuthStatus_PodlessState(t *testing.T) {
	tests := []struct {
		name      string
		phase     kyberv1.AgentPhase
		wantState runtimes.AuthState
	}{
		{"NeedsAuth offers device login", kyberv1.AgentPhaseNeedsAuth, runtimes.AuthAbsent},
		{"Starting keeps waiting for pod creation", kyberv1.AgentPhaseStarting, runtimes.AuthStarting},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestPublicServer(t, testAPIKey)
			agent := sampleAgentCRD("needy")
			agent.Spec.Runtime = "codex"
			agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
			agent.Status.Phase = tc.phase
			s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build()
			s.RestConfig = &rest.Config{}
			s.Clientset = k8sfake.NewSimpleClientset()

			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/needy/auth", nil)
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			rr := httptest.NewRecorder()
			buildTestHandler(s).ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var got runtimes.AuthObservation
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			if got.State != tc.wantState {
				t.Fatalf("state=%q, want %q", got.State, tc.wantState)
			}
		})
	}
}

func TestCodexDeviceAuthResetsCredentialAndStartsAgent(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "needy", Namespace: s.Namespace},
		Spec: kyberv1.AgentSpec{
			Runtime: "codex", DesiredPhase: kyberv1.AgentPhaseNeedsAuth,
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
		Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseNeedsAuth},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "needy-codex-auth", Namespace: s.Namespace},
		Data:       map[string][]byte{"auth.json": []byte(`{"expired":true}`)},
	}
	s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, secret).Build()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/needy/codex-device-auth", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	gotSecret := &corev1.Secret{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: secret.Name, Namespace: s.Namespace}, gotSecret); err != nil {
		t.Fatal(err)
	}
	if got := string(gotSecret.Data["auth.json"]); got != "{}" {
		t.Fatalf("auth.json=%q, want {}", got)
	}
	gotAgent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: agent.Name, Namespace: s.Namespace}, gotAgent); err != nil {
		t.Fatal(err)
	}
	if gotAgent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Fatalf("desiredPhase=%q, want Running", gotAgent.Spec.DesiredPhase)
	}
}

func TestCodexDeviceAuthCreatesLabeledCredentialAfterSwitch(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	agent := sampleAgentCRD("new-codex")
	agent.Spec.Runtime = "codex"
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeOAuth
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	agent.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithStatusSubresource(agent).WithObjects(agent).Build()

	req := authedRequest(t, http.MethodPost, "/api/v1/agents/new-codex/auth", nil)
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	secret := &corev1.Secret{}
	if err := s.K8sClient.Get(t.Context(), types.NamespacedName{Name: "new-codex-codex-auth", Namespace: s.Namespace}, secret); err != nil {
		t.Fatal(err)
	}
	if secret.Labels["kyber.io/agent"] != agent.Name {
		t.Fatalf("first-switch credential lacks cleanup label: %v", secret.Labels)
	}
}

func TestCodexDeviceAuthRejectsUnusedProviderCredentialForCustomInference(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	agent := sampleAgentCRD("endpoint-codex")
	agent.Spec.Runtime = "codex"
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeOAuth
	agent.Spec.Inference = &kyberv1.AgentInference{Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "endpoint-key", Key: "token"}}
	agent.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	if err := s.K8sClient.Create(t.Context(), agent); err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/endpoint-codex/auth", nil)
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := s.K8sClient.Get(t.Context(), types.NamespacedName{Name: "endpoint-codex-codex-auth", Namespace: s.Namespace}, &corev1.Secret{}); err == nil {
		t.Fatal("unused device credential was created")
	}
}

// MAT-8: the second and every later click of "Start device login".
//
// The endpoint signals "re-authorize me" by writing {} into <name>-codex-auth,
// and the controller's NeedsAuth recovery gate keys on that Secret's
// resourceVersion. On every retry the Secret ALREADY holds {} — it is written
// that way at agent creation for the device-auth path and nothing replaces it
// until a login actually succeeds — and Kubernetes does not bump
// resourceVersion for a byte-identical update. So the recorded claim still
// matched, the gate stayed shut, and the endpoint answered 204 while doing
// nothing. Reproduced on kyber dev 2026-08-26: click one moved the agent to
// Starting, click two left it in NeedsAuth forever.
//
// The fix is what an explicit Start already does — clear status.recoveryInput.
// Note the sibling test above deliberately seeds a DIFFERENT credential
// (`{"expired":true}`), so it never exercised this path.
func TestCodexDeviceAuthReopensGateWhenCredentialIsAlreadyPlaceholder(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	const claim = "rv:needy-codex-auth:672"
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "needy", Namespace: s.Namespace},
		Spec: kyberv1.AgentSpec{
			// Running, not NeedsAuth: the previous click already set it, so on
			// the retry the spec patch below is byte-identical too. The status
			// clear is then the ONLY write that changes the Agent — which
			// matters because the controller watches Agents and Pods, not
			// Secrets (SetupWithManager), so nothing else would wake it.
			Runtime: "codex", DesiredPhase: kyberv1.AgentPhaseRunning,
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
		Status: kyberv1.AgentStatus{
			Phase:         kyberv1.AgentPhaseNeedsAuth,
			RecoveryInput: claim,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "needy-codex-auth", Namespace: s.Namespace},
		// Already the placeholder — the state every retry starts from.
		Data: map[string][]byte{"auth.json": []byte("{}")},
	}
	s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).
		WithObjects(agent, secret).WithStatusSubresource(agent).Build()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/needy/codex-device-auth", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	gotAgent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(),
		types.NamespacedName{Name: agent.Name, Namespace: s.Namespace}, gotAgent); err != nil {
		t.Fatal(err)
	}
	// The whole defect: without this the claim still equals the live Secret's
	// resourceVersion, classifyEvent raises nothing, and the agent never leaves
	// NeedsAuth however many times the operator clicks.
	if gotAgent.Status.RecoveryInput != "" {
		t.Fatalf("recoveryInput=%q, want cleared — the gate must reopen for one attempt", gotAgent.Status.RecoveryInput)
	}
	if gotAgent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Fatalf("desiredPhase=%q, want Running", gotAgent.Spec.DesiredPhase)
	}
}
