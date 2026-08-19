package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/fleetdefaults"
)

const (
	fdNS = "kyber-system"
	fdCM = "kyber-fleet-defaults"
)

func newFDClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestHandleFleetDefaults_GET_ReturnsEmptyWhenConfigMapMissing(t *testing.T) {
	s := &Server{
		K8sClient:                  newFDClient(t),
		Namespace:                  fdNS,
		FleetDefaultsConfigMapName: fdCM,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet-defaults", nil)
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got FleetDefaultsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DefaultModel != "" || got.DefaultRuntimeVersion != "" {
		t.Errorf("expected empty, got %+v", got)
	}
}

func TestHandleFleetDefaults_GET_ReturnsConfigMapData(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: fdCM, Namespace: fdNS},
		Data: map[string]string{
			fleetdefaults.KeyDefaultModel:          "claude-sonnet-4-5",
			fleetdefaults.KeyDefaultRuntimeVersion: "2.1.119",
		},
	}
	s := &Server{
		K8sClient:                  newFDClient(t, cm),
		Namespace:                  fdNS,
		FleetDefaultsConfigMapName: fdCM,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet-defaults", nil)
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got FleetDefaultsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.DefaultModel != "claude-sonnet-4-5" {
		t.Errorf("DefaultModel = %q", got.DefaultModel)
	}
	if got.DefaultRuntimeVersion != "2.1.119" {
		t.Errorf("DefaultRuntimeVersion = %q", got.DefaultRuntimeVersion)
	}
}

func TestHandleFleetDefaults_GET_503WhenConfigMapNameUnset(t *testing.T) {
	s := &Server{K8sClient: newFDClient(t), Namespace: fdNS}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet-defaults", nil)
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestHandleFleetDefaults_PUT_CreatesConfigMapWhenMissing(t *testing.T) {
	c := newFDClient(t)
	s := &Server{K8sClient: c, Namespace: fdNS, FleetDefaultsConfigMapName: fdCM}
	body, _ := json.Marshal(FleetDefaultsResponse{DefaultModel: "claude-opus-4-7", DefaultRuntimeVersion: "2.1.200", CodexDefaultModel: "gpt-5.6-sol", CodexDefaultRuntimeVersion: "0.146.0"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet-defaults", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// Verify the ConfigMap was created with the expected data.
	got := &corev1.ConfigMap{}
	if err := c.Get(req.Context(), types.NamespacedName{Namespace: fdNS, Name: fdCM}, got); err != nil {
		t.Fatalf("Get after PUT: %v", err)
	}
	if got.Data[fleetdefaults.KeyDefaultModel] != "claude-opus-4-7" {
		t.Errorf("defaultModel = %q, want %q", got.Data[fleetdefaults.KeyDefaultModel], "claude-opus-4-7")
	}
	if got.Data[fleetdefaults.KeyDefaultRuntimeVersion] != "2.1.200" {
		t.Errorf("defaultRuntimeVersion = %q, want %q", got.Data[fleetdefaults.KeyDefaultRuntimeVersion], "2.1.200")
	}
	if got.Data[fleetdefaults.KeyCodexDefaultModel] != "gpt-5.6-sol" || got.Data[fleetdefaults.KeyCodexDefaultRuntimeVersion] != "0.146.0" {
		t.Errorf("Codex defaults not persisted: %+v", got.Data)
	}
	if got.Annotations[fleetDefaultsAuthoredAnnotation] != "true" {
		t.Errorf("authored annotation = %q, want true", got.Annotations[fleetDefaultsAuthoredAnnotation])
	}
}

func TestHandleFleetDefaults_PUT_PatchesExistingConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fdCM,
			Namespace: fdNS,
			Labels:    map[string]string{"helm.sh/chart": "kyber-1.3.7"},
		},
		Data: map[string]string{fleetdefaults.KeyDefaultModel: "old-model"},
	}
	c := newFDClient(t, cm)
	s := &Server{K8sClient: c, Namespace: fdNS, FleetDefaultsConfigMapName: fdCM}
	body, _ := json.Marshal(FleetDefaultsResponse{DefaultModel: "new-model", DefaultRuntimeVersion: "v9"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet-defaults", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	got := &corev1.ConfigMap{}
	if err := c.Get(req.Context(), types.NamespacedName{Namespace: fdNS, Name: fdCM}, got); err != nil {
		t.Fatalf("Get after PUT: %v", err)
	}
	if got.Data[fleetdefaults.KeyDefaultModel] != "new-model" {
		t.Errorf("defaultModel not updated: got %q", got.Data[fleetdefaults.KeyDefaultModel])
	}
	if got.Labels["helm.sh/chart"] != "kyber-1.3.7" {
		t.Errorf("chart label clobbered by PUT: got labels %v", got.Labels)
	}
}

func TestHandleFleetDefaults_PUT_LegacyClientPreservesCodexDefaults(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: fdCM, Namespace: fdNS},
		Data: map[string]string{
			fleetdefaults.KeyDefaultModel:               "old-claude",
			fleetdefaults.KeyCodexDefaultModel:          "gpt-5.6-sol",
			fleetdefaults.KeyCodexDefaultRuntimeVersion: "0.146.0",
		},
	}
	c := newFDClient(t, cm)
	s := &Server{K8sClient: c, Namespace: fdNS, FleetDefaultsConfigMapName: fdCM}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet-defaults", bytes.NewBufferString(
		`{"defaultModel":"new-claude","defaultRuntimeVersion":"2.1.221"}`))
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	got := &corev1.ConfigMap{}
	if err := c.Get(req.Context(), types.NamespacedName{Namespace: fdNS, Name: fdCM}, got); err != nil {
		t.Fatalf("Get after PUT: %v", err)
	}
	if got.Data[fleetdefaults.KeyCodexDefaultModel] != "gpt-5.6-sol" || got.Data[fleetdefaults.KeyCodexDefaultRuntimeVersion] != "0.146.0" {
		t.Errorf("legacy PUT cleared Codex defaults: %+v", got.Data)
	}
}

func TestHandleFleetDefaults_PUT_InvalidatesResolverCache(t *testing.T) {
	c := newFDClient(t)
	inv := &countingInvalidator{}
	s := &Server{
		K8sClient:                  c,
		Namespace:                  fdNS,
		FleetDefaultsConfigMapName: fdCM,
		FleetDefaultsInvalidator:   inv,
	}
	body, _ := json.Marshal(FleetDefaultsResponse{DefaultModel: "x"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet-defaults", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if inv.calls != 1 {
		t.Errorf("Invalidate called %d times, want 1", inv.calls)
	}
}

func TestHandleFleetDefaults_PUT_RejectsInvalidJSON(t *testing.T) {
	s := &Server{
		K8sClient:                  newFDClient(t),
		Namespace:                  fdNS,
		FleetDefaultsConfigMapName: fdCM,
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet-defaults", bytes.NewReader([]byte("{not json")))
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleFleetDefaults_RejectsUnsupportedMethod(t *testing.T) {
	s := &Server{
		K8sClient:                  newFDClient(t),
		Namespace:                  fdNS,
		FleetDefaultsConfigMapName: fdCM,
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/fleet-defaults", nil)
	rr := httptest.NewRecorder()
	s.handleFleetDefaults(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

type countingInvalidator struct{ calls int }

func (c *countingInvalidator) Invalidate() { c.calls++ }
