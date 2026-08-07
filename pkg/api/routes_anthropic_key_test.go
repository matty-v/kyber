package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func anthropicKeyTestServer(initialKey string) *Server {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kyber-anthropic-key",
			Namespace: "kyber-system",
		},
		Data: map[string][]byte{},
	}
	if initialKey != "" {
		secret.Data["api-key"] = []byte(initialKey)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	return &Server{
		K8sClient:              c,
		Namespace:              "kyber-system",
		AnthropicKeySecretName: "kyber-anthropic-key",
	}
}

func TestAnthropicKey_GET_ConfiguredFalseWhenAbsent(t *testing.T) {
	s := anthropicKeyTestServer("")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/anthropic-key", nil)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got anthropicKeyStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Configured {
		t.Errorf("expected configured=false when api-key is empty, got true")
	}
}

func TestAnthropicKey_GET_ConfiguredTrueWhenPresent(t *testing.T) {
	s := anthropicKeyTestServer("sk-ant-something")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/anthropic-key", nil)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got anthropicKeyStatusResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.Configured {
		t.Errorf("expected configured=true when api-key is present, got false")
	}
}

// TestAnthropicKey_GET_NeverEchoesKey: the most important security guard
// — even GET against an authenticated session must never return the key.
func TestAnthropicKey_GET_NeverEchoesKey(t *testing.T) {
	sensitiveKey := "sk-ant-this-must-not-leak-12345"
	s := anthropicKeyTestServer(sensitiveKey)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/anthropic-key", nil)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if strings.Contains(rr.Body.String(), sensitiveKey) {
		t.Fatalf("GET response leaked the api key: %s", rr.Body.String())
	}
}

func TestAnthropicKey_PUT_PatchesSecret(t *testing.T) {
	s := anthropicKeyTestServer("")
	body := bytes.NewBufferString(`{"key":"sk-ant-new"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/anthropic-key", body)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	// Verify the secret was actually patched.
	get := httptest.NewRequest(http.MethodGet, "/api/v1/settings/anthropic-key", nil)
	getRR := httptest.NewRecorder()
	s.handleAnthropicKeySetting(getRR, get)
	var status anthropicKeyStatusResponse
	_ = json.Unmarshal(getRR.Body.Bytes(), &status)
	if !status.Configured {
		t.Error("expected configured=true after PUT, got false")
	}
}

func TestAnthropicKey_PUT_RejectsEmpty(t *testing.T) {
	s := anthropicKeyTestServer("")
	body := bytes.NewBufferString(`{"key":""}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/anthropic-key", body)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestAnthropicKey_PUT_RejectsBadJSON(t *testing.T) {
	s := anthropicKeyTestServer("")
	body := bytes.NewBufferString(`not json`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/anthropic-key", body)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestAnthropicKey_PUT_ErrorBodyDoesNotEchoKey(t *testing.T) {
	// Even when the request fails (e.g., 400 on bad JSON containing the
	// key as a value-like string), the response must not echo it.
	s := anthropicKeyTestServer("")
	body := bytes.NewBufferString(`{"key": "sk-ant-leaked-on-error" garbage`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/anthropic-key", body)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "sk-ant-leaked-on-error") {
		t.Fatalf("error response leaked the api key: %s", rr.Body.String())
	}
}

func TestAnthropicKey_DELETE_ClearsKey(t *testing.T) {
	s := anthropicKeyTestServer("sk-ant-existing")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/anthropic-key", nil)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/settings/anthropic-key", nil)
	getRR := httptest.NewRecorder()
	s.handleAnthropicKeySetting(getRR, get)
	var status anthropicKeyStatusResponse
	_ = json.Unmarshal(getRR.Body.Bytes(), &status)
	if status.Configured {
		t.Error("expected configured=false after DELETE, got true")
	}
}

func TestAnthropicKey_UnsetSecretName_Returns503OnWrite(t *testing.T) {
	s := &Server{}
	body := bytes.NewBufferString(`{"key":"sk-ant"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/anthropic-key", body)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestAnthropicKey_UnsetSecretName_GETReturnsUnconfigured(t *testing.T) {
	// GET should NOT 503 when the server is in dev/test mode without a
	// configured Secret — the PWA still wants to render the settings card.
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/anthropic-key", nil)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got anthropicKeyStatusResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Configured {
		t.Errorf("expected configured=false on unconfigured server, got true")
	}
}

func TestAnthropicKey_PUT_RejectsOversizedBody(t *testing.T) {
	s := anthropicKeyTestServer("")
	big := strings.Repeat("a", maxAnthropicKeyBytes+10)
	body := bytes.NewBufferString(`{"key":"` + big + `"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/anthropic-key", body)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
}

func TestAnthropicKey_RejectsUnsupportedMethod(t *testing.T) {
	s := anthropicKeyTestServer("")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/anthropic-key", nil)
	rr := httptest.NewRecorder()
	s.handleAnthropicKeySetting(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}
