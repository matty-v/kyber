package api_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
)

func newJobEventsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := kyberv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func newJobEventsTestAgent(name string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber-system",
		},
	}
}

// TestInternalAPI_JobEvent_AppendsSuccess verifies the happy path: a
// well-formed POST is reflected in status.jobs on the Agent CR.
func TestInternalAPI_JobEvent_AppendsSuccess(t *testing.T) {
	scheme := newJobEventsScheme(t)
	agent := newJobEventsTestAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"jobName":"morning","startedAt":"2026-04-20T09:00:00Z","finishedAt":"2026-04-20T09:00:01Z","outcome":"success"}`
	resp, err := http.Post(ts.URL+"/internal/agents/chewie/job-events", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if len(got.Status.Jobs) != 1 {
		t.Fatalf("status.jobs len: got %d, want 1", len(got.Status.Jobs))
	}
	run := got.Status.Jobs[0]
	if run.JobName != "morning" {
		t.Errorf("JobName: got %q, want morning", run.JobName)
	}
	if run.Outcome != kyberv1.AgentJobOutcomeSuccess {
		t.Errorf("Outcome: got %q, want success", run.Outcome)
	}
	if run.StartedAt == nil || run.StartedAt.UTC().Format("15:04:05") != "09:00:00" {
		t.Errorf("StartedAt: got %v, want 09:00:00 UTC", run.StartedAt)
	}
}

// TestInternalAPI_JobEvent_RingBufferCapsPerJob verifies that exceeding
// MaxJobRunsPerJob entries drops the oldest entry FOR THE SAME job name,
// leaving entries for other jobs untouched.
func TestInternalAPI_JobEvent_RingBufferCapsPerJob(t *testing.T) {
	scheme := newJobEventsScheme(t)
	agent := newJobEventsTestAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Fire MaxJobRunsPerJob + 3 events for "morning" and 2 for "evening".
	// Expect: status.jobs contains 50 "morning" + 2 "evening" = 52 entries,
	// and the oldest "morning" entries (indices 0,1,2 in post-reach state)
	// should no longer be present.
	post := func(body string) {
		resp, err := http.Post(ts.URL+"/internal/agents/chewie/job-events", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status: got %d, want 204 (body=%s)", resp.StatusCode, body)
		}
	}

	for i := 0; i < api.MaxJobRunsPerJob+3; i++ {
		post(fmt.Sprintf(`{"jobName":"morning","startedAt":"2026-04-20T09:%02d:00Z","finishedAt":"2026-04-20T09:%02d:01Z","outcome":"success"}`, i%60, i%60))
	}
	post(`{"jobName":"evening","startedAt":"2026-04-20T18:00:00Z","finishedAt":"2026-04-20T18:00:01Z","outcome":"success"}`)
	post(`{"jobName":"evening","startedAt":"2026-04-20T18:01:00Z","finishedAt":"2026-04-20T18:01:01Z","outcome":"success"}`)

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}

	morningCount := 0
	eveningCount := 0
	for _, r := range got.Status.Jobs {
		switch r.JobName {
		case "morning":
			morningCount++
		case "evening":
			eveningCount++
		}
	}
	if morningCount != api.MaxJobRunsPerJob {
		t.Errorf("morning count: got %d, want %d (cap)", morningCount, api.MaxJobRunsPerJob)
	}
	if eveningCount != 2 {
		t.Errorf("evening count: got %d, want 2 (cap only evicts same-name)", eveningCount)
	}
}

// TestInternalAPI_JobEvent_InboundExcludedFromJobs verifies that job events
// for inbound-<requestID> dispatches are acknowledged (204) but NOT appended
// to status.jobs[] — those runs live in status.inboundRuns[] (capped per
// binding). Their unique-per-event names would otherwise defeat the per-name
// cap and grow the CR without bound (the Lando 2MB wedge, kyber#622).
func TestInternalAPI_JobEvent_InboundExcludedFromJobs(t *testing.T) {
	scheme := newJobEventsScheme(t)
	agent := newJobEventsTestAgent("lando")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(body string) int {
		resp, err := http.Post(ts.URL+"/internal/agents/lando/job-events", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Fire several distinct inbound-<id> events — each a unique name.
	for i := 0; i < 5; i++ {
		if sc := post(fmt.Sprintf(`{"jobName":"inbound-req%04d","startedAt":"2026-04-20T09:00:00Z","finishedAt":"2026-04-20T09:00:01Z","outcome":"success"}`, i)); sc != http.StatusNoContent {
			t.Fatalf("inbound POST status: got %d, want 204", sc)
		}
	}
	// A real scheduled job still records — proving we only skip inbound-*.
	if sc := post(`{"jobName":"compact-memory","startedAt":"2026-04-20T09:00:00Z","finishedAt":"2026-04-20T09:00:01Z","outcome":"success"}`); sc != http.StatusNoContent {
		t.Fatalf("scheduled POST status: got %d, want 204", sc)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "lando", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if len(got.Status.Jobs) != 1 {
		t.Fatalf("status.jobs len: got %d, want 1 (only compact-memory; inbound-* excluded)", len(got.Status.Jobs))
	}
	if got.Status.Jobs[0].JobName != "compact-memory" {
		t.Errorf("retained job: got %q, want compact-memory", got.Status.Jobs[0].JobName)
	}
}

// TestInternalAPI_JobEvent_GlobalTotalCap verifies the MaxJobRunsTotal
// backstop: even with a high-cardinality set of distinct (non-inbound) job
// names that each stay under the per-name cap, status.jobs[] never exceeds
// the global total. Runaway guard for the CR-size invariant (kyber#622).
func TestInternalAPI_JobEvent_GlobalTotalCap(t *testing.T) {
	scheme := newJobEventsScheme(t)
	agent := newJobEventsTestAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Fire MaxJobRunsTotal + 25 events, each under a UNIQUE job name so the
	// per-name cap never trims — only the global backstop can bound this.
	total := api.MaxJobRunsTotal + 25
	for i := 0; i < total; i++ {
		body := fmt.Sprintf(`{"jobName":"job-%05d","startedAt":"2026-04-20T09:00:00Z","finishedAt":"2026-04-20T09:00:01Z","outcome":"success"}`, i)
		resp, err := http.Post(ts.URL+"/internal/agents/chewie/job-events", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status: got %d, want 204", resp.StatusCode)
		}
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if len(got.Status.Jobs) != api.MaxJobRunsTotal {
		t.Errorf("status.jobs len: got %d, want %d (global cap)", len(got.Status.Jobs), api.MaxJobRunsTotal)
	}
	// The oldest entries must have been evicted — the most recent name must
	// still be present, the very first must be gone.
	names := map[string]bool{}
	for _, r := range got.Status.Jobs {
		names[r.JobName] = true
	}
	if names["job-00000"] {
		t.Errorf("oldest entry job-00000 should have been evicted by the global cap")
	}
	if !names[fmt.Sprintf("job-%05d", total-1)] {
		t.Errorf("newest entry should be retained")
	}
}

// TestInternalAPI_JobEvent_Rejects verifies bad inputs produce 4xx, not 5xx.
func TestInternalAPI_JobEvent_Rejects(t *testing.T) {
	scheme := newJobEventsScheme(t)
	agent := newJobEventsTestAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"empty body", ``, http.StatusBadRequest},
		{"missing jobName", `{"startedAt":"2026-04-20T09:00:00Z","outcome":"success"}`, http.StatusBadRequest},
		{"bad outcome", `{"jobName":"x","startedAt":"2026-04-20T09:00:00Z","outcome":"ok"}`, http.StatusBadRequest},
		{"bad time", `{"jobName":"x","startedAt":"not-a-time","outcome":"success"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/internal/agents/chewie/job-events", "application/json", bytes.NewBufferString(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("status: got %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

// TestInternalAPI_JobEvent_Concurrent verifies that parallel POSTs all
// land in status.jobs (no lost updates). Regression guard for the race
// where both handlers read the same status.jobs, each appends locally,
// and the later patch clobbers the earlier one.
func TestInternalAPI_JobEvent_Concurrent(t *testing.T) {
	scheme := newJobEventsScheme(t)
	agent := newJobEventsTestAgent("chewie")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const parallel = 20
	var wg sync.WaitGroup
	errs := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Unique startedAt per call so tests don't accidentally count a single
			// append twice if dedup logic were ever added.
			body := fmt.Sprintf(`{"jobName":"morning","startedAt":"2026-04-20T09:%02d:00Z","finishedAt":"2026-04-20T09:%02d:01Z","outcome":"success"}`, i, i)
			resp, err := http.Post(ts.URL+"/internal/agents/chewie/job-events", "application/json", bytes.NewBufferString(body))
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				errs <- fmt.Errorf("status %d on iter %d", resp.StatusCode, i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("parallel POST: %v", err)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatalf("Get agent: %v", err)
	}
	if len(got.Status.Jobs) != parallel {
		t.Errorf("status.jobs len: got %d, want %d — race lost updates", len(got.Status.Jobs), parallel)
	}
}

// TestInternalAPI_JobEvent_AgentNotFound returns 404 without panicking when
// the POST targets an agent that doesn't exist.
func TestInternalAPI_JobEvent_AgentNotFound(t *testing.T) {
	scheme := newJobEventsScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"jobName":"morning","startedAt":"2026-04-20T09:00:00Z","outcome":"success"}`
	resp, err := http.Post(ts.URL+"/internal/agents/ghost/job-events", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}
