package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/oauth/mockserver"
)

// MAT-90 G7: storing the credential wakes the controller, whose status
// writes race the start patch. Once the Secret is saved the one-time code is
// spent, so the handler must retry the conflict and never answer 500.
func oauthStartHarness(t *testing.T, desired kyberv1.AgentPhase, conflicts int) (http.Handler, client.Client, *int, string) {
	t.Helper()
	mock := mockserver.New()
	mockSrv := httptest.NewServer(mock)
	t.Cleanup(mockSrv.Close)
	verifier := "test-verifier-value"
	code := mock.IssueCode(pkceChallenge(verifier))

	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "racer", Namespace: "kyber-system"},
		Spec:       kyberv1.AgentSpec{Machine: "testvm", Runtime: "claude-code", DesiredPhase: desired},
		Status:     kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseNeedsAuth},
	}
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeOAuth
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "racer-oauth", Namespace: "kyber-system"},
		Data:       map[string][]byte{"refresh_token": []byte("old-token")},
	}
	patches := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(mustNewScheme(t)).
		WithRuntimeObjects(defaultMachine(), agent, secret).
		WithStatusSubresource(agent).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isAgent := obj.(*kyberv1.Agent); isAgent {
				patches++
				if patches <= conflicts {
					return k8serrors.NewConflict(schema.GroupResource{Group: "kyber.io", Resource: "agents"}, obj.GetName(), nil)
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		}}).
		Build()
	s := &api.Server{
		K8sClient:         fakeClient,
		APIKey:            testAPIKey,
		Namespace:         "kyber-system",
		ValidRuntimes:     map[string]bool{"claude-code": true},
		AnthropicTokenURL: mockSrv.URL + "/v1/oauth/token",
	}
	return s.BuildHandler(), fakeClient, &patches, code
}

func postOAuth(t *testing.T, h http.Handler, code string) *httptest.ResponseRecorder {
	t.Helper()
	req := authedRequest(t, http.MethodPost, "/api/v1/agents/racer/oauth", map[string]string{
		"oauthCode": code, "pkceVerifier": "test-verifier-value", "state": "s",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestReauthorize_RetriesStartPatchConflict(t *testing.T) {
	h, c, patches, code := oauthStartHarness(t, kyberv1.AgentPhaseNeedsAuth, 1)
	rr := postOAuth(t, h, code)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want 204 after retrying the conflict", rr.Code, rr.Body.String())
	}
	if *patches != 2 {
		t.Fatalf("agent patches=%d, want 2 (one conflict, one retry)", *patches)
	}
	got := &kyberv1.Agent{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "racer", Namespace: "kyber-system"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Fatalf("desiredPhase=%q, want Running", got.Spec.DesiredPhase)
	}
}

// The conflicting write is often one the cache-backed client has not seen,
// so retries must span more than a handful of immediate attempts.
func TestReauthorize_OutlastsASlowCache(t *testing.T) {
	h, _, patches, code := oauthStartHarness(t, kyberv1.AgentPhaseNeedsAuth, 7)
	if rr := postOAuth(t, h, code); rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want 204 after 7 conflicts", rr.Code, rr.Body.String())
	}
	if *patches != 8 {
		t.Fatalf("agent patches=%d, want 8", *patches)
	}
}

func TestReauthorize_AlreadyRunningNeedsNoPatch(t *testing.T) {
	h, _, patches, code := oauthStartHarness(t, kyberv1.AgentPhaseRunning, 99)
	rr := postOAuth(t, h, code)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want 204", rr.Code, rr.Body.String())
	}
	if *patches != 0 {
		t.Fatalf("agent patches=%d, want none when desiredPhase is already Running", *patches)
	}
}

func TestReauthorize_ChangedAgentKeepsCredentialAnd409(t *testing.T) {
	h, c, _, code := oauthStartHarness(t, kyberv1.AgentPhaseStopped, 0)
	rr := postOAuth(t, h, code)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rr.Code, rr.Body.String())
	}
	secret := &corev1.Secret{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "racer-oauth", Namespace: "kyber-system"}, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["refresh_token"]) == "old-token" || len(secret.Data["access_token"]) == 0 {
		t.Fatal("credential was not saved before the 409")
	}
}

func TestReauthorize_PersistentConflictIsNot500(t *testing.T) {
	h, _, _, code := oauthStartHarness(t, kyberv1.AgentPhaseNeedsAuth, 99)
	rr := postOAuth(t, h, code)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 once retries are exhausted", rr.Code, rr.Body.String())
	}
}
