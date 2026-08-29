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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
)

type fixedAgentMetrics struct{ sample api.AgentContainerMetrics }

func (f fixedAgentMetrics) AgentContainer(context.Context, string, string) (api.AgentContainerMetrics, error) {
	return f.sample, nil
}

type failingAgentMetrics struct{}

func (failingAgentMetrics) AgentContainer(context.Context, string, string) (api.AgentContainerMetrics, error) {
	return api.AgentContainerMetrics{}, fmt.Errorf("metrics unavailable")
}

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

func TestStatusEvent_ResourceUsageUpdatesActivity(t *testing.T) {
	scheme := statusEventTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
		Spec:       kyberv1.AgentSpec{Resources: kyberv1.AgentResources{Disk: resource.MustParse("100")}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"type":"resource_usage","at":"2026-08-27T12:00:00Z","resources":{"cpuUsageMillicores":750,"cpuLimitMillicores":2000,"memoryUsedBytes":1048576,"memoryLimitBytes":2097152,"diskUsedBytes":90,"diskTotalBytes":100,"diskReserveReached":true,"diskUsageMethod":"directory","diskUsageState":"ready","diskUsedSampledAt":"2026-08-27T11:58:00Z"}}`
	resp, err := http.Post(ts.URL+"/internal/agents/alice/status-event", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
		t.Fatal(err)
	}
	usage := got.Status.Activity.Resources
	if usage == nil || usage.CPUUsageMillicores != 750 || usage.DiskUsedBytes != 90 || !usage.DiskReserveReached {
		t.Fatalf("resource usage not patched: %+v", usage)
	}
	if usage.DiskUsageMethod != "directory" || usage.DiskUsageState != "ready" || usage.DiskUsedSampledAt == nil {
		t.Errorf("disk accounting metadata not patched: %+v", usage)
	}
	if got.Status.Activity.LastHeartbeatAt == nil || usage.SampledAt.Time != got.Status.Activity.LastHeartbeatAt.Time {
		t.Errorf("sample and heartbeat timestamps differ: %+v", got.Status.Activity)
	}
}

func TestStatusEvent_ResourceUsageUsesAgentMetricsAndRequestedLimits(t *testing.T) {
	scheme := statusEventTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{Resources: kyberv1.AgentResources{
			CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), Disk: resource.MustParse("2Gi"),
		}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"),
		api.WithAgentMetricsProvider(fixedAgentMetrics{api.AgentContainerMetrics{
			CPUUsageMillicores: 83, MemoryUsedBytes: 682 * 1024 * 1024,
		}}))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"type":"resource_usage","at":"2026-08-27T12:00:00Z","resources":{"cpuUsageMillicores":1,"cpuLimitMillicores":100,"memoryUsedBytes":3145728,"memoryLimitBytes":67108864,"diskUsedBytes":90,"diskTotalBytes":100}}`
	resp, err := http.Post(ts.URL+"/internal/agents/alice/status-event", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
		t.Fatal(err)
	}
	usage := got.Status.Activity.Resources
	if usage.CPUUsageMillicores != 83 || usage.CPULimitMillicores == nil || *usage.CPULimitMillicores != 1000 {
		t.Errorf("cpu = %+v", usage)
	}
	if usage.MemoryUsedBytes != 682*1024*1024 || usage.MemoryLimitBytes == nil || *usage.MemoryLimitBytes != 1024*1024*1024 {
		t.Errorf("memory = %+v", usage)
	}
	if usage.DiskUsedBytes != 90 || usage.DiskTotalBytes != 2*1024*1024*1024 {
		t.Errorf("disk sample changed: %+v", usage)
	}
	if usage.DiskReserveReached {
		t.Error("legacy sample without accounting state must not trigger reserve")
	}
}

func TestStatusEvent_ResourceUsagePreservesAgentUsageWhenMetricsDisappear(t *testing.T) {
	scheme := statusEventTestScheme(t)
	cpuLimit, memoryLimit := int64(1000), int64(1024*1024*1024)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
		Spec:       kyberv1.AgentSpec{Resources: kyberv1.AgentResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), Disk: resource.MustParse("100")}},
		Status: kyberv1.AgentStatus{Activity: &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{
			CPUUsageMillicores: 83, CPULimitMillicores: &cpuLimit,
			MemoryUsedBytes: 682 * 1024 * 1024, MemoryLimitBytes: &memoryLimit,
		}}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).WithStatusSubresource(&kyberv1.Agent{}).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"), api.WithAgentMetricsProvider(failingAgentMetrics{}))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body := `{"type":"resource_usage","at":"2026-08-27T12:01:00Z","resources":{"cpuUsageMillicores":0,"cpuLimitMillicores":100,"memoryUsedBytes":5242880,"memoryLimitBytes":67108864,"diskUsedBytes":91,"diskTotalBytes":100}}`
	resp, err := http.Post(ts.URL+"/internal/agents/alice/status-event", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
		t.Fatal(err)
	}
	usage := got.Status.Activity.Resources
	if usage.CPUUsageMillicores != 83 || usage.MemoryUsedBytes != 682*1024*1024 || usage.DiskUsedBytes != 91 {
		t.Fatalf("terminal sample = %+v", usage)
	}
	if *usage.CPULimitMillicores != 1000 || *usage.MemoryLimitBytes != 1024*1024*1024 {
		t.Fatalf("requested limits changed: %+v", usage)
	}
}

// An unmeasured sample (pending/error) carries the previous decision forward,
// while a measured one — ready or partial — moves the lifecycle in either
// direction through the hysteresis band. The old name of this test,
// ...PreservesExhaustionUntilReady, described the rule that made DiskExhausted
// permanent on agents whose walk is always partial.
func TestStatusEvent_DiskReserveMovesOnMeasuredSamplesOnly(t *testing.T) {
	scheme := statusEventTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{Resources: kyberv1.AgentResources{
			CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), Disk: resource.MustParse("100"),
		}},
		Status: kyberv1.AgentStatus{Activity: &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{
			DiskUsedBytes: 95, DiskTotalBytes: 100, DiskReserveReached: true,
			DiskUsageMethod: "directory", DiskUsageState: "ready",
		}}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).WithStatusSubresource(&kyberv1.Agent{}).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(state string, used int) *kyberv1.AgentResourceUsage {
		t.Helper()
		body := fmt.Sprintf(`{"type":"resource_usage","at":"2026-08-27T12:01:00Z","resources":{"diskUsedBytes":%d,"diskTotalBytes":100,"diskUsageMethod":"directory","diskUsageState":%q}}`, used, state)
		resp, err := http.Post(ts.URL+"/internal/agents/alice/status-event", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status for %s sample: got %d", state, resp.StatusCode)
		}
		got := &kyberv1.Agent{}
		if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
			t.Fatal(err)
		}
		return got.Status.Activity.Resources
	}

	if usage := post("error", 95); !usage.DiskReserveReached {
		t.Fatalf("error sample cleared exhaustion: %+v", usage)
	}
	if usage := post("pending", 0); !usage.DiskReserveReached {
		t.Fatalf("pending sample cleared exhaustion: %+v", usage)
	}
	if usage := post("partial", 95); !usage.DiskReserveReached {
		t.Fatalf("partial sample above reserve did not preserve exhaustion: %+v", usage)
	}
	// 85 is inside the hysteresis band, so the previous decision stands.
	if usage := post("partial", 85); !usage.DiskReserveReached {
		t.Fatalf("sample inside the hysteresis band flapped the reserve: %+v", usage)
	}
	// 80 is at the clear ratio. This previously asserted the opposite — that a
	// partial sample could never clear exhaustion — which is what made
	// DiskExhausted permanent for every agent with rootfs persistence, because
	// their walk is always partial and the "ready" escape never arrived.
	if usage := post("partial", 80); usage.DiskReserveReached {
		t.Fatalf("partial sample below the clear ratio left the agent exhausted: %+v", usage)
	}
	if usage := post("ready", 95); !usage.DiskReserveReached {
		t.Fatalf("ready sample above reserve did not exhaust: %+v", usage)
	}
	if usage := post("ready", 80); usage.DiskReserveReached {
		t.Fatalf("ready sample below reserve did not clear exhaustion: %+v", usage)
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

// After an online PVC grow the control plane must decide on the allocation the
// SIDECAR measured against, not the spec's new larger one.
//
// KYBER_AGENT_DISK_BYTES is a pod env var read once at sidecar start, so a
// resize never reaches a running pod. Deciding on the spec value would clear the
// Agent phase while the sidecar — still dividing by the old allocation — keeps
// the runtime paused through its marker. The agent would report Running and
// answer nothing, which is the failure shape this whole lifecycle exists to make
// visible rather than to create.
func TestStatusEvent_DiskReserveUsesTheAllocationTheSidecarMeasured(t *testing.T) {
	scheme := statusEventTestScheme(t)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
		// Spec has already been grown to 400; the running sidecar still has 100.
		Spec: kyberv1.AgentSpec{Resources: kyberv1.AgentResources{
			CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), Disk: resource.MustParse("400"),
		}},
		Status: kyberv1.AgentStatus{Activity: &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{
			DiskUsedBytes: 95, DiskTotalBytes: 100, DiskReserveReached: true,
			DiskUsageMethod: "directory", DiskUsageState: "partial",
		}}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).WithStatusSubresource(&kyberv1.Agent{}).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 95 of the sidecar's 100 is still exhausted; 95 of the spec's 400 would not be.
	body := `{"type":"resource_usage","at":"2026-08-27T12:01:00Z","resources":{"diskUsedBytes":95,"diskTotalBytes":100,"diskUsageMethod":"directory","diskUsageState":"partial"}}`
	resp, err := http.Post(ts.URL+"/internal/agents/alice/status-event", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status-event: got %d", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
		t.Fatal(err)
	}
	usage := got.Status.Activity.Resources
	if !usage.DiskReserveReached {
		t.Fatalf("a grown spec released the phase while the sidecar still measures 95%%: %+v", usage)
	}
	// The reported total still follows the spec, so the operator sees the new size.
	if usage.DiskTotalBytes != 400 {
		t.Fatalf("reported total did not follow the grown allocation: %+v", usage)
	}
}
