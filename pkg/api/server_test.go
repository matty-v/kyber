package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// newTestPublicServer builds a Server with a fake k8s client and a fixed API key for testing.
func newTestPublicServer(t *testing.T, apiKey string) *api.Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme kyberv1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	return &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        apiKey,
		Namespace:     "kyber-system",
	}
}

// buildTestHandler creates an httptest.Server using the public Server's Handler method.
// Since Server.Start blocks and uses net.Listen, we use the internal handler for unit tests.
func buildTestHandler(s *api.Server) http.Handler {
	return s.BuildHandler()
}

// TestMiddleware_RequestIDIsSet verifies that every response carries X-Request-ID.
func TestMiddleware_RequestIDIsSet(t *testing.T) {
	s := newTestPublicServer(t, "test-key")
	h := buildTestHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if id := rr.Header().Get(api.RequestIDHeader); id == "" {
		t.Error("expected X-Request-ID header to be set")
	}
}

// TestMiddleware_RecoverPanic is tested in middleware_test.go (internal package)
// because recoverMiddleware is unexported. The test there injects a panicking
// handler directly and verifies 500 is returned without propagating the panic.

// TestMiddleware_AuthMissingKey verifies 401 is returned when Authorization header is absent.
func TestMiddleware_AuthMissingKey(t *testing.T) {
	s := newTestPublicServer(t, "my-secret-key")
	h := buildTestHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rr.Code)
	}
}

// TestMiddleware_AuthInvalidKey verifies 401 for a wrong API key.
func TestMiddleware_AuthInvalidKey(t *testing.T) {
	s := newTestPublicServer(t, "my-secret-key")
	h := buildTestHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rr.Code)
	}
}

// TestMiddleware_AuthValidKey verifies that a correct key passes through to the route.
func TestMiddleware_AuthValidKey(t *testing.T) {
	s := newTestPublicServer(t, "my-secret-key")
	h := buildTestHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer my-secret-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// The fake client returns an empty list — we get 200.
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHealthzNoAuth verifies /healthz responds with 200 without any auth header.
// Health probes are sent by the kubelet without auth — they must bypass the API key check.
func TestHealthzNoAuth(t *testing.T) {
	s := newTestPublicServer(t, "my-secret-key")
	h := buildTestHandler(s)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for /healthz without auth, got %d", rr.Code)
	}
}

// TestMiddleware_WebhooksBypassAuth verifies that /webhooks/* doesn't need an API key.
func TestMiddleware_WebhooksBypassAuth(t *testing.T) {
	s := newTestPublicServer(t, "my-secret-key")
	h := buildTestHandler(s)

	// POST to a webhook with a non-existent agent — no auth header.
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/dave", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// Should not return 401 — webhook routes bypass API key auth.
	if rr.Code == http.StatusUnauthorized {
		t.Errorf("webhook route returned 401 — it should bypass API key auth")
	}
}
