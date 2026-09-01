package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
)

// buildRotateAPIKeyHandler returns a Server with the rotate handler wired
// against a fake K8sClient seeded with the api-credentials Secret. Returns
// the server, the underlying handler, and the seed key so tests can
// authenticate the initial request and inspect the post-rotation state.
func buildRotateAPIKeyHandler(t *testing.T, secretName, namespace, seedKey string) (*api.Server, http.Handler, *fake.ClientBuilder) {
	t.Helper()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"api-key":        []byte(seedKey),
			"webhook-secret": []byte("untouched-webhook"),
		},
	}
	cb := fake.NewClientBuilder().WithObjects(secret)
	srv := &api.Server{
		K8sClient:        cb.Build(),
		APIKey:           seedKey,
		APIKeySecretName: secretName,
		Namespace:        namespace,
		Recorder:         record.NewFakeRecorder(8),
	}
	return srv, srv.BuildHandler(), cb
}

func TestRotateAPIKey_HappyPath(t *testing.T) {
	const ns = "kyber-system"
	const secretName = "kyber-api-credentials"
	const seedKey = "old-seed-key"

	srv, handler, _ := buildRotateAPIKeyHandler(t, secretName, ns, seedKey)

	// Seed key authenticates the initial rotate request.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rotate-api-key", nil)
	req.Header.Set("Authorization", "Bearer "+seedKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.APIKey) != 64 { // 32 bytes hex = 64 chars
		t.Fatalf("expected 64-char hex key, got %d chars: %q", len(resp.APIKey), resp.APIKey)
	}
	if resp.APIKey == seedKey {
		t.Fatalf("rotated key matches seed key (no rotation happened)")
	}

	// Secret was patched with the new key; webhook-secret is untouched.
	got := &corev1.Secret{}
	if err := srv.K8sClient.Get(context.Background(), types.NamespacedName{Name: secretName, Namespace: ns}, got); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(got.Data["api-key"]) != resp.APIKey {
		t.Fatalf("Secret api-key not updated: got %q, want %q", got.Data["api-key"], resp.APIKey)
	}
	if string(got.Data["webhook-secret"]) != "untouched-webhook" {
		t.Fatalf("webhook-secret was clobbered: %q", got.Data["webhook-secret"])
	}

	// Old key returns 401 on the next request.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req2.Header.Set("Authorization", "Bearer "+seedKey)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on old key, got %d", rec2.Code)
	}

	// New key authenticates.
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req3.Header.Set("Authorization", "Bearer "+resp.APIKey)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code == http.StatusUnauthorized {
		t.Fatalf("new key was rejected: %d %s", rec3.Code, rec3.Body.String())
	}
}

func TestRotateAPIKey_GeneratesUniqueKeys(t *testing.T) {
	const ns = "kyber-system"
	const secretName = "kyber-api-credentials"
	const seedKey = "seed"

	srv, handler, _ := buildRotateAPIKeyHandler(t, secretName, ns, seedKey)

	keys := make(map[string]bool)
	currentKey := seedKey
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rotate-api-key", nil)
		req.Header.Set("Authorization", "Bearer "+currentKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("rotation %d: expected 200, got %d body=%s", i, rec.Code, rec.Body.String())
		}
		var resp struct {
			APIKey string `json:"apiKey"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("rotation %d: decode: %v", i, err)
		}
		if keys[resp.APIKey] {
			t.Fatalf("rotation %d returned duplicate key %q", i, resp.APIKey)
		}
		keys[resp.APIKey] = true
		currentKey = resp.APIKey
	}
	_ = srv
}

func TestRotateAPIKey_ReturnsServiceUnavailable_WhenSecretNameUnset(t *testing.T) {
	const seedKey = "seed-key"
	srv := &api.Server{
		K8sClient: fake.NewClientBuilder().Build(),
		APIKey:    seedKey,
		Namespace: "kyber-system",
		// APIKeySecretName intentionally empty.
		Recorder: record.NewFakeRecorder(4),
	}
	handler := srv.BuildHandler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rotate-api-key", nil)
	req.Header.Set("Authorization", "Bearer "+seedKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rotation_unavailable") {
		t.Fatalf("expected rotation_unavailable error code, got %s", rec.Body.String())
	}
}

func TestRotateAPIKey_RejectsNonPOST(t *testing.T) {
	const seedKey = "seed"
	_, handler, _ := buildRotateAPIKeyHandler(t, "kyber-api-credentials", "kyber-system", seedKey)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/rotate-api-key", nil)
	req.Header.Set("Authorization", "Bearer "+seedKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestRotateAPIKey_RequiresAuth(t *testing.T) {
	const seedKey = "seed"
	_, handler, _ := buildRotateAPIKeyHandler(t, "kyber-api-credentials", "kyber-system", seedKey)

	// Missing Authorization header.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rotate-api-key", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (missing auth), got %d", rec.Code)
	}

	// Wrong key.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/rotate-api-key", nil)
	req2.Header.Set("Authorization", "Bearer wrong-key")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (wrong key), got %d", rec2.Code)
	}
}

// TestRotateAPIKey_BrowserSessionSurvivesForTheRotatingBrowserOnly: rotation is
// the only lever that signs other browsers out, so both halves of it matter —
// the browser that pressed the button must stay in, and every other cookie
// must stop working. Since MAT-38 the second half is implicit (cookies are
// signed with a key derived from the API key, so changing the key invalidates
// them), which is exactly why it needs a test at this level rather than a
// trust in an explicit revoke call that no longer exists.
func TestRotateAPIKey_BrowserSessionSurvivesForTheRotatingBrowserOnly(t *testing.T) {
	const ns = "kyber-system"
	const secretName = "kyber-api-credentials"
	const seedKey = "old-seed-key"
	const origin = "https://kyber.example"

	_, handler, _ := buildRotateAPIKeyHandler(t, secretName, ns, seedKey)

	sessionCookie := func(t *testing.T) *http.Cookie {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, origin+"/api/v1/browser-session", nil)
		req.Header.Set("Authorization", "Bearer "+seedKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("exchange status = %d, want 204: %s", rec.Code, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == "kyber_browser_session" {
				return c
			}
		}
		t.Fatal("no session cookie issued")
		return nil
	}

	authenticates := func(c *http.Cookie) bool {
		req := httptest.NewRequest(http.MethodGet, origin+"/api/v1/config", nil)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code != http.StatusUnauthorized
	}

	rotating := sessionCookie(t)
	bystander := sessionCookie(t)
	if !authenticates(rotating) || !authenticates(bystander) {
		t.Fatal("fresh session cookies do not authenticate")
	}

	req := httptest.NewRequest(http.MethodPost, origin+"/api/v1/rotate-api-key", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", origin)
	req.AddCookie(rotating)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var replacement *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "kyber_browser_session" {
			replacement = c
		}
	}
	if replacement == nil {
		t.Fatal("rotation did not re-issue a session cookie to the initiating browser")
	}
	if replacement.Value == rotating.Value {
		t.Error("replacement cookie is the pre-rotation token")
	}
	if !authenticates(replacement) {
		t.Error("the rotating browser was signed out by its own rotation")
	}
	if authenticates(rotating) {
		t.Error("the pre-rotation cookie still authenticates")
	}
	if authenticates(bystander) {
		t.Error("another browser's session survived the rotation")
	}
}
