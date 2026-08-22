package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimedetect"
)

// Write-path model validation (canary regression 2026-08-22): a typo'd
// model in fleet-defaults or set-model used to write cleanly and then
// fail every agent turn while the platform showed green. These tests
// pin the reject/accept/force matrix.

func snapshotCache(t *testing.T, claudeIDs, codexIDs []string) runtimedetect.Cache {
	t.Helper()
	c := runtimedetect.NewMemoryCache()
	snap := &runtimedetect.Snapshot{}
	for _, id := range claudeIDs {
		snap.Models = append(snap.Models, runtimedetect.Model{ID: id})
	}
	for _, id := range codexIDs {
		snap.CodexModels = append(snap.CodexModels, runtimedetect.Model{ID: id})
	}
	if err := c.Put(context.Background(), snap); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return c
}

func TestValidateModelValue(t *testing.T) {
	full := snapshotCache(t, []string{"claude-sonnet-5", "claude-fable-5"}, []string{"gpt-5.6-sol"})
	empty := runtimedetect.NewMemoryCache()

	tests := []struct {
		name       string
		cache      runtimedetect.Cache
		runtime    string
		model      string
		wantReject bool
	}{
		{name: "empty model always ok", cache: full, runtime: "claude-code", model: "", wantReject: false},
		{name: "known model ok", cache: full, runtime: "claude-code", model: "claude-sonnet-5", wantReject: false},
		{name: "known model with 1m suffix ok", cache: full, runtime: "claude-code", model: "claude-sonnet-5[1m]", wantReject: false},
		{name: "unknown claude model rejected", cache: full, runtime: "claude-code", model: "claude-opus-4-canary-marker", wantReject: true},
		{name: "alias without prefix ok", cache: full, runtime: "claude-code", model: "sonnet", wantReject: false},
		{name: "codex known ok", cache: full, runtime: "codex", model: "gpt-5.6-sol", wantReject: false},
		{name: "codex unknown rejected", cache: full, runtime: "codex", model: "gpt-nonexistent", wantReject: true},
		{name: "claude id not checked against codex catalog", cache: full, runtime: "codex", model: "claude-sonnet-5", wantReject: false},
		{name: "no catalog data accepts anything", cache: empty, runtime: "claude-code", model: "claude-opus-4-canary-marker", wantReject: false},
		{name: "nil cache accepts anything", cache: nil, runtime: "claude-code", model: "claude-whatever", wantReject: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{RuntimeDetectCache: tt.cache}
			msg := s.validateModelValue(context.Background(), tt.runtime, tt.model, "")
			if (msg != "") != tt.wantReject {
				t.Fatalf("validateModelValue(%s, %q) = %q, wantReject=%v", tt.runtime, tt.model, msg, tt.wantReject)
			}
		})
	}
}

func TestValidateModelValue_AgentCatalogExtendsAcceptance(t *testing.T) {
	// The snapshot doesn't know the model but the agent's own
	// authenticated catalog does — entitlements differ per account.
	cache := snapshotCache(t, []string{"claude-sonnet-5"}, nil)
	if err := cache.(runtimedetect.AgentCatalogCache).PutAgentModels(context.Background(),
		"wedge", []runtimedetect.Model{{ID: "claude-secret-preview"}}); err != nil {
		t.Fatalf("PutAgentModels: %v", err)
	}
	s := &Server{RuntimeDetectCache: cache}
	if msg := s.validateModelValue(context.Background(), "claude-code", "claude-secret-preview", "wedge"); msg != "" {
		t.Fatalf("agent-catalog model rejected: %s", msg)
	}
	if msg := s.validateModelValue(context.Background(), "claude-code", "claude-secret-preview", ""); msg == "" {
		t.Fatal("model unknown to the snapshot should reject without the agent catalog")
	}
}

func TestFleetDefaultsPut_RejectsUnknownModel(t *testing.T) {
	s := &Server{
		K8sClient:                  newFDClient(t),
		Namespace:                  fdNS,
		FleetDefaultsConfigMapName: fdCM,
		RuntimeDetectCache:         snapshotCache(t, []string{"claude-sonnet-5"}, nil),
	}
	body := `{"defaultModel":"claude-opus-4-canary-marker","defaultRuntimeVersion":"latest"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet-defaults", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "claude-opus-4-canary-marker") {
		t.Errorf("error should name the rejected model: %s", rr.Body.String())
	}
}

func TestFleetDefaultsPut_ForceWritesUnknownModel(t *testing.T) {
	s := &Server{
		K8sClient:                  newFDClient(t),
		Namespace:                  fdNS,
		FleetDefaultsConfigMapName: fdCM,
		RuntimeDetectCache:         snapshotCache(t, []string{"claude-sonnet-5"}, nil),
	}
	body := `{"defaultModel":"claude-brand-new-model","defaultRuntimeVersion":"latest","force":true}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet-defaults", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with force; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFleetDefaultsPut_AcceptsKnownModel(t *testing.T) {
	s := &Server{
		K8sClient:                  newFDClient(t),
		Namespace:                  fdNS,
		FleetDefaultsConfigMapName: fdCM,
		RuntimeDetectCache:         snapshotCache(t, []string{"claude-sonnet-5"}, nil),
	}
	body := `{"defaultModel":"claude-sonnet-5","defaultRuntimeVersion":"latest"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet-defaults", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func newSetModelServer(t *testing.T, cache runtimedetect.Cache) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: fdNS},
		Spec:       kyberv1.AgentSpec{Runtime: "claude-code"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()
	return &Server{K8sClient: fakeClient, Namespace: fdNS, RuntimeDetectCache: cache}
}

func TestSetModel_RejectsUnknownModel(t *testing.T) {
	s := newSetModelServer(t, snapshotCache(t, []string{"claude-sonnet-5"}, nil))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/wedge/set-model",
		bytes.NewBufferString(`{"model":"claude-opus-4-canary-marker"}`))
	rr := httptest.NewRecorder()
	s.setAgentModel(rr, req, "wedge")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestSetModel_ForceBypassesValidation(t *testing.T) {
	s := newSetModelServer(t, snapshotCache(t, []string{"claude-sonnet-5"}, nil))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/wedge/set-model",
		bytes.NewBufferString(`{"model":"claude-brand-new-model","force":true}`))
	rr := httptest.NewRecorder()
	s.setAgentModel(rr, req, "wedge")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with force; body=%s", rr.Code, rr.Body.String())
	}
}

func TestSetModel_AcceptsKnownModel(t *testing.T) {
	s := newSetModelServer(t, snapshotCache(t, []string{"claude-sonnet-5"}, nil))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/wedge/set-model",
		bytes.NewBufferString(`{"model":"claude-sonnet-5"}`))
	rr := httptest.NewRecorder()
	s.setAgentModel(rr, req, "wedge")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}
