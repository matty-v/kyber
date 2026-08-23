package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLoggingSettings(t *testing.T) {
	s := &Server{LoggingGlobalLevel: "info", LoggingComponentLevels: map[string]string{"status-sidecar": "debug"}, LoggingArchiveRetention: 30}
	rr := httptest.NewRecorder()
	s.handleLoggingSettings(rr, httptest.NewRequest(http.MethodGet, "/api/v1/logging/settings", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var got loggingSettingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.GlobalLevel != "info" || got.ComponentOverrides["status-sidecar"] != "debug" || got.ArchiveRetentionDays != 30 || got.ManagedBy != "helm" {
		t.Errorf("response = %+v", got)
	}
}

func TestLoggingTargetsDiscoversManagedPodsAndContainers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	managed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent-sol", Namespace: "kyber-system", UID: types.UID("uid-1"),
			Labels: map[string]string{"app.kubernetes.io/part-of": "kyber", "app.kubernetes.io/component": "agent", "kyber.io/agent": "sol"},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "session-brief"}},
			Containers:     []corev1.Container{{Name: "agent"}, {Name: "kyber-status-sidecar"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	unmanaged := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "kyber-system"}}
	s := &Server{
		K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(managed, unmanaged).Build(),
		Namespace: "kyber-system", LoggingGlobalLevel: "info",
		LoggingComponentLevels: map[string]string{"status-sidecar": "debug"},
	}
	rr := httptest.NewRecorder()
	s.handleLoggingTargets(rr, httptest.NewRequest(http.MethodGet, "/api/v1/logging/targets", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Targets []loggingTargetResponse `json:"targets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("target count = %d, want 1: %s", len(got.Targets), rr.Body.String())
	}
	target := got.Targets[0]
	if target.Workload != "sol" || target.PodUID != "uid-1" || len(target.Containers) != 3 {
		t.Errorf("target = %+v", target)
	}
	if gotLevel := target.Containers[2].EffectiveLevel; gotLevel != "debug" {
		t.Errorf("status sidecar effective level = %q, want debug", gotLevel)
	}
	if target.Containers[1].ManagedLevel {
		t.Error("agent runtime must report unmanaged verbosity")
	}
}

func TestLoggingRoutesRequireAuthentication(t *testing.T) {
	s := &Server{APIKey: "test-key"}
	for _, path := range []string{"/api/v1/logging/settings", "/api/v1/logging/targets"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.BuildHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rr.Code)
			}
		})
	}
}

func TestLoggingRoutesRejectUnsupportedMethods(t *testing.T) {
	tests := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/api/v1/logging/settings", (&Server{}).handleLoggingSettings},
		{"/api/v1/logging/targets", (&Server{}).handleLoggingTargets},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			tc.handler(rr, httptest.NewRequest(http.MethodPost, tc.path, nil))
			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rr.Code)
			}
		})
	}
}
