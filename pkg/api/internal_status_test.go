package api_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
)

// statusEventTestScheme builds a scheme with corev1 + Kyber CRD types
// so the fake client can Get/Patch Agent objects in status-event tests.
func statusEventTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	if err := kyberv1.AddToScheme(s); err != nil {
		t.Fatalf("kyberv1 AddToScheme: %v", err)
	}
	return s
}

// TestStatusEvent_HeartbeatUpdatesLastHeartbeatAt verifies that a
// heartbeat POST patches the agent's status.activity.lastHeartbeatAt
// field. This is the foundation behavior of the kyber-status-sidecar
// (kyber#248).
func TestStatusEvent_HeartbeatUpdatesLastHeartbeatAt(t *testing.T) {
	scheme := statusEventTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	heartbeatAt := "2026-05-03T21:30:00Z"
	body := fmt.Sprintf(`{"type":"heartbeat","at":"%s"}`, heartbeatAt)
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/internal/agents/alice/status-event",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if got.Status.Activity == nil || got.Status.Activity.LastHeartbeatAt == nil {
		t.Fatal("status.activity.lastHeartbeatAt not patched")
	}
	want, _ := time.Parse(time.RFC3339, heartbeatAt)
	if !got.Status.Activity.LastHeartbeatAt.Time.Equal(want) {
		t.Errorf("LastHeartbeatAt: got %v, want %v",
			got.Status.Activity.LastHeartbeatAt.Time, want)
	}
}

// TestStatusEvent_AgentNotFound returns 404 cleanly when the agent does
// not exist. The sidecar uses 404 as a signal to back off rather than
// hammering retries.
func TestStatusEvent_AgentNotFound(t *testing.T) {
	scheme := statusEventTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/internal/agents/missing/status-event",
		strings.NewReader(`{"type":"heartbeat","at":"2026-05-03T21:30:00Z"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestStatusEvent_MalformedBodyReturns400 — bad JSON in the request
// bounces with VALIDATION 400 rather than a server error.
func TestStatusEvent_MalformedBodyReturns400(t *testing.T) {
	scheme := statusEventTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/internal/agents/alice/status-event",
		strings.NewReader(`not json`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestStatusEvent_UnknownTypeIgnored — forward-compat: a future event
// kind shouldn't break older control planes. Unknown types return 204
// (accepted, no patch).
func TestStatusEvent_UnknownTypeIgnored(t *testing.T) {
	scheme := statusEventTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"type":"some-future-event","at":"2026-05-03T21:30:00Z"}`
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/internal/agents/alice/status-event",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status: got %d, want 204 (forward-compat ignore)", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	// Activity status should remain nil — unknown event must not patch.
	if got.Status.Activity != nil && got.Status.Activity.LastHeartbeatAt != nil {
		t.Errorf("unknown event type should not patch activity; got %+v", got.Status.Activity)
	}
}

// TestStatusEvent_MemoryOOMUpdatesLastKernelOOMKillAt pins kyber#285:
// a memory_oom event from the sidecar (cgroup memory.events.local
// oom_kill counter increment) stamps Status.LastKernelOOMKillAt so the
// agent controller can route the agent to MemoryExhausted regardless
// of whether kubelet tagged the container's State.Terminated.Reason.
func TestStatusEvent_MemoryOOMUpdatesLastKernelOOMKillAt(t *testing.T) {
	scheme := statusEventTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	oomAt := "2026-05-08T01:23:45Z"
	body := fmt.Sprintf(`{"type":"memory_oom","at":"%s","oomKillCount":3}`, oomAt)
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/internal/agents/alice/status-event",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if got.Status.LastKernelOOMKillAt == nil {
		t.Fatal("status.lastKernelOOMKillAt not patched")
	}
	want, _ := time.Parse(time.RFC3339, oomAt)
	if !got.Status.LastKernelOOMKillAt.Time.Equal(want) {
		t.Errorf("LastKernelOOMKillAt: got %v, want %v",
			got.Status.LastKernelOOMKillAt.Time, want)
	}
}
