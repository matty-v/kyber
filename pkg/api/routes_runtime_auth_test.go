package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestRuntimeAPIKeyAuthorizationCreatesTargetSecretAfterSwitch(t *testing.T) {
	for _, tc := range []struct{ runtime, secret string }{
		{"claude-code", "anthropic"},
		{"codex", "openai"},
		{"hermes", "openrouter"},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			agent := sampleAgentCRD("new-target")
			agent.UID = types.UID("new-target-uid")
			agent.Spec.Runtime = tc.runtime
			agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
			agent.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
			agent.Status.Phase = kyberv1.AgentPhaseNeedsAuth
			agent.Status.RecoveryInput = "old-source-credential"
			s := newTestPublicServer(t, testAPIKey)
			s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithStatusSubresource(agent).Build()
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
			if stored.Labels["kyber.io/agent"] != agent.Name {
				t.Fatalf("target credential lacks cleanup label: %v", stored.Labels)
			}
			if len(stored.OwnerReferences) != 1 || stored.OwnerReferences[0].UID != agent.UID {
				t.Fatalf("target credential lacks agent owner: %v", stored.OwnerReferences)
			}
			updated := &kyberv1.Agent{}
			if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
				t.Fatalf("desiredPhase=%q", updated.Spec.DesiredPhase)
			}
			if updated.Status.RecoveryInput != "" {
				t.Fatalf("recovery gate remained closed: %q", updated.Status.RecoveryInput)
			}
		})
	}
}

func TestRuntimeAPIKeyAuthorizationRequiresNeedsAuth(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	agent := sampleAgentCRD("running-target")
	agent.Spec.Runtime = "hermes"
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	if err := s.K8sClient.Create(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/running-target/auth", map[string]string{"apiKey": "new-provider-key"})
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: "running-target-openrouter", Namespace: "kyber-system"}, &corev1.Secret{}); err == nil {
		t.Fatal("credential changed outside NeedsAuth")
	}
}

func TestRuntimeAPIKeyAuthorizationUsesInferenceCredential(t *testing.T) {
	s := newTestPublicServer(t, testAPIKey)
	agent := sampleAgentCRD("endpoint-target")
	agent.Spec.Runtime = "hermes"
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	agent.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	agent.Spec.Inference = &kyberv1.AgentInference{Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: "endpoint-key", Key: "token"}}
	if err := s.K8sClient.Create(t.Context(), agent); err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/endpoint-target/auth", map[string]string{"apiKey": "unused-provider-key"})
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := s.K8sClient.Get(t.Context(), types.NamespacedName{Name: "endpoint-target-openrouter", Namespace: s.Namespace}, &corev1.Secret{}); err == nil {
		t.Fatal("unused provider credential was created")
	}
}
