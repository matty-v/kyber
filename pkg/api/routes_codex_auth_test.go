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
