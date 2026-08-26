package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

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
