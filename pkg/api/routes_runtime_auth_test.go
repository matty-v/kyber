package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// An agent whose intent is already Running may leave NeedsAuth on the new
// credential before the handler reads it back; that is success, not a 409.
func TestRuntimeAPIKeyAuthorizationAgentAlreadyStarting(t *testing.T) {
	agent := sampleAgentCRD("quick")
	agent.UID = types.UID("quick-uid")
	agent.Spec.Runtime = "hermes"
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	agent.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	stored := false
	s := newTestPublicServer(t, testAPIKey)
	s.K8sClient = fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithStatusSubresource(agent).WithObjects(agent).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					stored = true
				}
				return c.Create(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if a, ok := obj.(*kyberv1.Agent); ok && err == nil && stored {
					a.Status.Phase = kyberv1.AgentPhaseCreating // the controller moved on
				}
				return err
			},
		}).Build()
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/quick/auth", map[string]string{"apiKey": "new-provider-key"})
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want 204", rr.Code, rr.Body.String())
	}
}
