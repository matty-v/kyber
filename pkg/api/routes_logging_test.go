package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakePlatformArchiveReader struct {
	selection  GenericArchiveSelection
	selections []GenericArchiveSelection
	records    []string
	err        error
}

func (f *fakePlatformArchiveReader) ListContainerSelections(_ context.Context, limit int) ([]GenericArchiveSelection, error) {
	if limit > 0 && len(f.selections) > limit {
		return f.selections[:limit], f.err
	}
	return f.selections, f.err
}

func (f *fakePlatformArchiveReader) ReadContainerLines(_ context.Context, selection GenericArchiveSelection, _, _ time.Time) (ReadResult, error) {
	f.selection = selection
	return ReadResult{Lines: []LogLine{{Text: "archived line"}}}, nil
}

func (f *fakePlatformArchiveReader) StreamContainerRecords(_ context.Context, selection GenericArchiveSelection, _, _ time.Time, emit func(string, LogLine) error) error {
	f.selection = selection
	for _, raw := range f.records {
		line, _ := parseArchiveLine(raw)
		if err := emit(raw, line); err != nil {
			return err
		}
	}
	return f.err
}

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
	if target.Workload != "sol" || target.PodUID != "uid-1" || len(target.Containers) != 3 || len(target.Sources) != 1 {
		t.Errorf("target = %+v", target)
	}
	if gotLevel := target.Containers[2].EffectiveLevel; gotLevel != "debug" {
		t.Errorf("status sidecar effective level = %q, want debug", gotLevel)
	}
	if target.Containers[1].ManagedLevel {
		t.Error("agent runtime must report unmanaged verbosity")
	}
}

func TestLoggingTargetsIncludesRetainedReplacedPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "control-plane-new", Namespace: "kyber-system", UID: types.UID("uid-new"), Labels: map[string]string{
		"app.kubernetes.io/part-of": "kyber", "app.kubernetes.io/component": "control-plane",
	}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "control-plane"}}}}
	reader := &fakePlatformArchiveReader{selections: []GenericArchiveSelection{
		{Component: "control-plane", Workload: "control-plane", PodUID: "uid-old", Container: "control-plane"},
		{Component: "control-plane", Workload: "control-plane", PodUID: "uid-new", Container: "control-plane"},
	}}
	s := &Server{K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(), Namespace: "kyber-system", PlatformArchiveReader: reader}
	rr := httptest.NewRecorder()
	s.handleLoggingTargets(rr, httptest.NewRequest(http.MethodGet, "/api/v1/logging/targets", nil))
	var got struct {
		Targets []loggingTargetResponse `json:"targets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("targets = %+v, want live and retained", got.Targets)
	}
	for _, target := range got.Targets {
		if target.Workload != "control-plane" {
			t.Errorf("target = %+v, want stable chart workload identity", target)
		}
	}
}

func TestArchivedLoggingTargetsRetainResourceIdentity(t *testing.T) {
	targets := (&Server{Namespace: "kyber-system"}).archivedLoggingTargets([]GenericArchiveSelection{
		{Component: "agent", Workload: "sol", PodUID: "agent-uid", Container: "agent"},
		{Component: "node-agent", Workload: "machine-1", PodUID: "node-uid", Container: "node-agent"},
	}, nil)
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want agent and machine", targets)
	}
	for _, target := range targets {
		switch target.Component {
		case "agent":
			if target.Agent != "sol" {
				t.Errorf("agent target = %+v, want agent identity sol", target)
			}
		case "node-agent":
			if target.Machine != "machine-1" {
				t.Errorf("node target = %+v, want machine identity machine-1", target)
			}
		}
	}
}

func TestLoggingRoutesRequireAuthentication(t *testing.T) {
	s := &Server{APIKey: "test-key"}
	for _, path := range []string{"/api/v1/logging/settings", "/api/v1/logging/targets", "/api/v1/logging/logs", "/api/v1/logging/export"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.BuildHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rr.Code)
			}
		})
	}
}

func TestLoggingLogsValidatesDiscoveredTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent-sol", Namespace: "kyber-system", UID: types.UID("uid-1"),
			Labels: map[string]string{"app.kubernetes.io/part-of": "kyber"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}},
	}
	s := &Server{
		K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		Clientset: k8sfake.NewSimpleClientset(), Namespace: "kyber-system",
	}
	tests := []struct {
		name string
		url  string
		code int
	}{
		{"missing identity", "/api/v1/logging/logs", http.StatusBadRequest},
		{"stale uid", "/api/v1/logging/logs?pod=agent-sol&podUid=old&container=agent", http.StatusNotFound},
		{"unknown container", "/api/v1/logging/logs?pod=agent-sol&podUid=uid-1&container=secret-sidecar", http.StatusNotFound},
		{"tail cap", "/api/v1/logging/logs?pod=agent-sol&podUid=uid-1&container=agent&tail=10001", http.StatusBadRequest},
		{"invalid since", "/api/v1/logging/logs?pod=agent-sol&podUid=uid-1&container=agent&since=yesterday", http.StatusBadRequest},
		{"archive deferred", "/api/v1/logging/logs?pod=agent-sol&podUid=uid-1&container=agent&source=archive", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.handleLoggingLogs(rr, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if rr.Code != tc.code {
				t.Errorf("status = %d, want %d: %s", rr.Code, tc.code, rr.Body.String())
			}
		})
	}
}

func TestPodHasContainerIncludesInitAndRegular(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "setup"}},
		Containers:     []corev1.Container{{Name: "agent"}},
	}}
	for _, name := range []string{"setup", "agent"} {
		if !loggingPodHasContainer(pod, name) {
			t.Errorf("podHasContainer(%q) = false, want true", name)
		}
	}
	if loggingPodHasContainer(pod, "missing") {
		t.Error("podHasContainer(missing) = true, want false")
	}
}

func TestLoggingLogsReadsGenericArchive(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent-sol", Namespace: "kyber-system", UID: types.UID("uid-1"),
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "kyber", "app.kubernetes.io/component": "agent", "kyber.io/agent": "sol",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}},
	}
	reader := &fakePlatformArchiveReader{selections: []GenericArchiveSelection{{Component: "agent", Workload: "sol", PodUID: "uid-1", Container: "agent"}}}
	s := &Server{
		K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		Namespace: "kyber-system", PlatformArchiveReader: reader,
	}
	url := "/api/v1/logging/logs?pod=agent-sol&podUid=uid-1&container=agent&component=agent&workload=sol&source=archive&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z"
	rr := httptest.NewRecorder()
	s.handleLoggingLogs(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "archived line\n" {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
	if reader.selection.Workload != "sol" || reader.selection.PodUID != "uid-1" || reader.selection.Container != "agent" {
		t.Errorf("selection = %+v", reader.selection)
	}
}

func TestLoggingExportStreamsAndSignalsLimit(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-sol", Namespace: "kyber-system", UID: types.UID("uid-1"), Labels: map[string]string{
			"app.kubernetes.io/part-of": "kyber", "app.kubernetes.io/component": "agent", "kyber.io/agent": "sol",
		}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}},
	}
	reader := &fakePlatformArchiveReader{selections: []GenericArchiveSelection{{Component: "agent", Workload: "sol", PodUID: "uid-1", Container: "agent"}}, records: []string{
		`{"timestamp":"2026-06-03T10:00:00Z","message":"first"}`,
		`{"timestamp":"2026-06-03T10:01:00Z","message":"second"}`,
	}}
	s := &Server{
		K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(), Namespace: "kyber-system",
		PlatformArchiveReader: reader, MaxExportBytes: 6,
	}
	url := "/api/v1/logging/export?pod=agent-sol&podUid=uid-1&container=agent&component=agent&workload=sol&format=text&since=2026-06-03T09:00:00Z&until=2026-06-03T11:00:00Z"
	rr := httptest.NewRecorder()
	s.handleLoggingExport(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "first\n") || !strings.Contains(rr.Body.String(), "truncated") {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "kyber-agent-sol-agent.log") {
		t.Errorf("Content-Disposition = %q", got)
	}
}

func TestLoggingRoutesRejectUnsupportedMethods(t *testing.T) {
	tests := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/api/v1/logging/settings", (&Server{}).handleLoggingSettings},
		{"/api/v1/logging/targets", (&Server{}).handleLoggingTargets},
		{"/api/v1/logging/logs", (&Server{}).handleLoggingLogs},
		{"/api/v1/logging/export", (&Server{}).handleLoggingExport},
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
