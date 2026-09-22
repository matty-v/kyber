package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestRuntimeAPIKeyAuthorizationCreatesTargetSecretAfterSwitch(t *testing.T) {
	for _, tc := range []struct{ runtime, secret string }{
		{"claude-code", "anthropic"},
		{"codex", "openai"},
		{"hermes", "openrouter"},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			s := newTestPublicServer(t, testAPIKey)
			agent := sampleAgentCRD("new-target")
			agent.Spec.Runtime = tc.runtime
			agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
			agent.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
			agent.Status.Phase = kyberv1.AgentPhaseNeedsAuth
			if err := s.K8sClient.Create(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			req := authedRequest(t, http.MethodPost, "/api/v1/agents/new-target/auth", map[string]string{"apiKey": "new-provider-key"})
			rr := httptest.NewRecorder()
			buildTestHandler(s).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			stored := &corev1.Secret{}
			if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: "new-target-" + tc.secret, Namespace: "kyber-system"}, stored); err != nil {
				t.Fatal(err)
			}
			if string(stored.Data["token"]) != "new-provider-key" {
				t.Fatal("target credential missing")
			}
			updated := &kyberv1.Agent{}
			if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
				t.Fatalf("desiredPhase=%q", updated.Spec.DesiredPhase)
			}
		})
	}
}
