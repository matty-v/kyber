package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/podtoken"
)

var testSigningKey = []byte("kyber-566-test-internal-signing-key-0001")

// newAuthServer builds an InternalServer with internal-auth wired (HMAC
// authenticator over testSigningKey) plus a fake kube client preloaded with the
// given objects. graceMode toggles accept-and-log-unauthenticated. The fake
// client is returned so tests can assert side effects (AC4: secret unchanged).
func newAuthServer(t *testing.T, graceMode bool, objs ...client.Object) (*httptest.Server, client.Client, func()) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	srv := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"),
		api.WithInternalAuth(api.NewHMACInternalAuthenticator(testSigningKey), graceMode),
	)
	ts := httptest.NewServer(srv.Handler())
	return ts, fakeClient, ts.Close
}

// do issues a request to the auth server with an optional Bearer token.
func do(t *testing.T, ts *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// AC1: the internal API authenticates callers — an unauthenticated request is rejected.
func TestInternalAuth_Unauthenticated_401(t *testing.T) {
	ts, _, cleanup := newAuthServer(t, false)
	defer cleanup()

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"garbage token", "not-a-valid-token"},
		{"wrong-key token", podtoken.Sign("dave", []byte("a-totally-different-key-zzzzzzzzzzzz"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, ts, http.MethodGet, "/internal/agents/dave/session-brief", tc.token, "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status: got %d, want 401", resp.StatusCode)
			}
		})
	}
}

// AC1/AC6: a valid token authenticates the caller for its OWN resource.
func TestInternalAuth_SelfToken_Allowed(t *testing.T) {
	ts, _, cleanup := newAuthServer(t, false)
	defer cleanup()

	resp := do(t, ts, http.MethodGet, "/internal/agents/dave/session-brief", podtoken.Sign("dave", testSigningKey), "")
	defer resp.Body.Close()
	// No brief stored → 404, but crucially NOT 401/403: auth + self-authz passed.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("self token should pass auth, got %d", resp.StatusCode)
	}
}

// AC2: agent A acting on agent B's resource is rejected with 403.
func TestInternalAuth_CrossAgent_403(t *testing.T) {
	ts, _, cleanup := newAuthServer(t, false)
	defer cleanup()

	resp := do(t, ts, http.MethodGet, "/internal/agents/eve/session-brief", podtoken.Sign("dave", testSigningKey), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-agent read: got %d, want 403", resp.StatusCode)
	}
}

// AC5: telemetry / state-change endpoints cannot be spoofed on behalf of another agent.
func TestInternalAuth_CrossAgentTelemetry_403(t *testing.T) {
	ts, _, cleanup := newAuthServer(t, false)
	defer cleanup()

	daveTok := podtoken.Sign("dave", testSigningKey)
	for _, path := range []string{
		"/internal/agents/eve/status-event",
		"/internal/agents/eve/status",
		"/internal/agents/eve/token-usage",
		"/internal/agents/eve/job-events",
		"/internal/agents/eve/runtime-version",
	} {
		t.Run(path, func(t *testing.T) {
			resp := do(t, ts, http.MethodPost, path, daveTok, `{}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s with dave's token: got %d, want 403", path, resp.StatusCode)
			}
		})
	}
}

// AC4 (headline): agent A cannot overwrite agent B's OAuth secret cross-agent,
// and the 403 short-circuits BEFORE any write — eve's secret is unchanged.
func TestInternalAuth_CrossAgentOAuthOverwrite_DeniedAndUnchanged(t *testing.T) {
	eveSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "eve-oauth", Namespace: "kyber-system"},
		Data:       map[string][]byte{"refresh_token": []byte("eve-original-rt")},
	}
	ts, c, cleanup := newAuthServer(t, false, eveSecret)
	defer cleanup()

	resp := do(t, ts, http.MethodPost, "/internal/agents/eve/refresh-token",
		podtoken.Sign("dave", testSigningKey), `{"refresh_token":"attacker-controlled"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-agent OAuth overwrite: got %d, want 403", resp.StatusCode)
	}

	got := &corev1.Secret{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "eve-oauth", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get eve-oauth: %v", err)
	}
	if string(got.Data["refresh_token"]) != "eve-original-rt" {
		t.Fatalf("eve's refresh_token was mutated by a cross-agent caller: %q", got.Data["refresh_token"])
	}
}

// Same guarantee for the Codex credential endpoint (kyber#681): it writes a
// full credential document into another agent's Secret, so a cross-agent caller
// must be refused before any write lands.
func TestInternalAuth_CrossAgentCodexAuthOverwrite_DeniedAndUnchanged(t *testing.T) {
	original := `{"tokens":{"refresh_token":"eve-original"}}`
	eveSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "eve-codex-auth", Namespace: "kyber-system"},
		Data:       map[string][]byte{"auth.json": []byte(original)},
	}
	ts, c, cleanup := newAuthServer(t, false, eveSecret)
	defer cleanup()

	resp := do(t, ts, http.MethodPost, "/internal/agents/eve/codex-auth",
		podtoken.Sign("dave", testSigningKey),
		`{"auth_json":"{\"tokens\":{\"refresh_token\":\"attacker-controlled\"}}"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-agent codex-auth overwrite: got %d, want 403", resp.StatusCode)
	}

	got := &corev1.Secret{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "eve-codex-auth", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get eve-codex-auth: %v", err)
	}
	if string(got.Data["auth.json"]) != original {
		t.Fatalf("eve's codex credential was mutated by a cross-agent caller: %q", got.Data["auth.json"])
	}
}

// Node-agent identity is admitted to machine routes, rejected on agent routes;
// an agent token is rejected on machine routes.
func TestInternalAuth_NodeAgentIdentity(t *testing.T) {
	ts, _, cleanup := newAuthServer(t, false)
	defer cleanup()
	naTok := podtoken.Sign(podtoken.NodeAgentIdentity, testSigningKey)

	t.Run("node-agent allowed on machine route", func(t *testing.T) {
		resp := do(t, ts, http.MethodPost, "/internal/machines/m1/preemption-notice", naTok, `{"instanceId":"i-1"}`)
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Fatalf("node-agent on machine route should pass auth, got %d", resp.StatusCode)
		}
	})

	t.Run("node-agent rejected on agent route", func(t *testing.T) {
		resp := do(t, ts, http.MethodGet, "/internal/agents/dave/session-brief", naTok, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("node-agent on agent route: got %d, want 403", resp.StatusCode)
		}
	})

	t.Run("agent token rejected on machine route", func(t *testing.T) {
		resp := do(t, ts, http.MethodPost, "/internal/machines/m1/preemption-notice",
			podtoken.Sign("dave", testSigningKey), `{"instanceId":"i-1"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("agent token on machine route: got %d, want 403", resp.StatusCode)
		}
	})
}

// Grace mode softens only the UNAUTHENTICATED case (migration window): a missing
// token is accepted-and-logged, but a valid token used cross-agent is STILL 403.
func TestInternalAuth_GraceMode(t *testing.T) {
	ts, _, cleanup := newAuthServer(t, true)
	defer cleanup()

	t.Run("missing token accepted in grace", func(t *testing.T) {
		resp := do(t, ts, http.MethodGet, "/internal/agents/dave/session-brief", "", "")
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Fatalf("grace mode must accept unauthenticated, got 401")
		}
	})

	t.Run("cross-agent still denied in grace", func(t *testing.T) {
		resp := do(t, ts, http.MethodGet, "/internal/agents/eve/session-brief",
			podtoken.Sign("dave", testSigningKey), "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("grace mode must still deny cross-agent, got %d", resp.StatusCode)
		}
	})
}

// Fail-closed (kyber#566 revision, Matt's call): when auth is REQUIRED but no
// signing key was delivered, the :8082 surface must refuse to serve — every
// internal route 503s, with or without a token — so a misconfigured deploy
// cannot silently re-open the agent→agent hole. Distinct from the back-compat
// "no option wired" mode below (which intentionally does not gate).
func TestInternalAuth_FailClosed_MissingKey_503(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	srv := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithKubeClient(c, "kyber-system"),
		api.WithInternalAuthFailClosed(),
	)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := []struct {
		method, path, token string
	}{
		{http.MethodGet, "/internal/agents/dave/session-brief", ""},
		// Even a token signed with what WOULD be the key is refused — there is
		// no authenticator to verify against, so nothing is trusted.
		{http.MethodGet, "/internal/agents/dave/session-brief", podtoken.Sign("dave", testSigningKey)},
		{http.MethodPost, "/internal/agents/dave/refresh-token", ""},
		{http.MethodPost, "/internal/machines/m1/preemption-notice", ""},
		{http.MethodPost, "/internal/nodes/n1/resources", ""},
	}
	for _, tc := range cases {
		resp := do(t, ts, tc.method, tc.path, tc.token, `{}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s: got %d, want 503 (fail-closed)", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// Without WithInternalAuth, the server is unauthenticated (back-compat for the
// pre-#566 behavior and the migration's first phase).
func TestInternalAuth_Disabled_NoEnforcement(t *testing.T) {
	srv := api.NewInternalServer(briefstore.NewMemoryStore())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp := do(t, ts, http.MethodGet, "/internal/agents/dave/session-brief", "", "")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("auth-disabled server should not gate, got %d", resp.StatusCode)
	}
}
