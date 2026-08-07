package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// TestAgentAPI_PatchJobs_ReplacesSpecJobs verifies a PATCH with a Jobs
// pointer replaces spec.jobs wholesale.
func TestAgentAPI_PatchJobs_ReplacesSpecJobs(t *testing.T) {
	agent := sampleAgentCRD("chewie")
	agent.Spec.Jobs = []kyberv1.AgentJob{
		{Name: "existing", Schedule: "0 0 * * *", Prompt: "old"},
	}
	handler, fakeClient := buildAgentHandler(t, agent)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := `{"jobs":[{"name":"morning","schedule":"0 9 * * *","prompt":"hello","exclusive":true}]}`
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/agents/chewie", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes := readAll(t, resp)
		t.Fatalf("status: got %d, want 200 (body: %s)", resp.StatusCode, bodyBytes)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(), k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Jobs) != 1 {
		t.Fatalf("spec.jobs len: got %d, want 1", len(got.Spec.Jobs))
	}
	if got.Spec.Jobs[0].Name != "morning" || got.Spec.Jobs[0].Schedule != "0 9 * * *" || !got.Spec.Jobs[0].Exclusive {
		t.Errorf("spec.jobs[0]: got %+v, want {morning, 0 9 * * *, exclusive=true}", got.Spec.Jobs[0])
	}
}

// TestAgentAPI_PatchJobs_EmptySliceClears verifies that an empty array
// (not nil) clears spec.jobs.
func TestAgentAPI_PatchJobs_EmptySliceClears(t *testing.T) {
	agent := sampleAgentCRD("chewie")
	agent.Spec.Jobs = []kyberv1.AgentJob{{Name: "old", Schedule: "0 0 * * *", Prompt: "x"}}
	handler, fakeClient := buildAgentHandler(t, agent)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := `{"jobs":[]}`
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/agents/chewie", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	_ = fakeClient.Get(context.Background(), k8stypes.NamespacedName{Name: "chewie", Namespace: "kyber-system"}, got)
	if len(got.Spec.Jobs) != 0 {
		t.Errorf("spec.jobs: expected empty, got %+v", got.Spec.Jobs)
	}
}

// TestAgentAPI_PatchJobs_Rejects covers the validation paths before touching k8s.
func TestAgentAPI_PatchJobs_Rejects(t *testing.T) {
	agent := sampleAgentCRD("chewie")
	handler, _ := buildAgentHandler(t, agent)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantField  string
	}{
		{"bad name", `{"jobs":[{"name":"Has Spaces","schedule":"0 0 * * *","prompt":"x"}]}`, 400, "jobs"},
		{"duplicate names", `{"jobs":[{"name":"a","schedule":"0 0 * * *","prompt":"x"},{"name":"a","schedule":"0 0 * * *","prompt":"y"}]}`, 400, "jobs"},
		{"wrong field count", `{"jobs":[{"name":"a","schedule":"0 0 * *","prompt":"x"}]}`, 400, "jobs"},
		{"empty prompt", `{"jobs":[{"name":"a","schedule":"0 0 * * *","prompt":""}]}`, 400, "jobs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/agents/chewie", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("PATCH: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status: got %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

// TestAgentAPI_GetAgent_IncludesJobs verifies GET returns jobs + lastJobRuns.
// LastJobRuns should carry one entry per name, picking the LATER entry when
// duplicates exist in status.jobs (oldest-first order → index-based last-wins).
func TestAgentAPI_GetAgent_IncludesJobs(t *testing.T) {
	agent := sampleAgentCRD("chewie")
	agent.Spec.Jobs = []kyberv1.AgentJob{
		{Name: "morning", Schedule: "0 9 * * *", Prompt: "hi"},
	}
	now := metav1.Now()
	older := metav1.NewTime(now.Add(-1e9))
	agent.Status.Jobs = []kyberv1.AgentJobRun{
		{JobName: "morning", StartedAt: &older, Outcome: kyberv1.AgentJobOutcomeFailed},
		{JobName: "morning", StartedAt: &now, Outcome: kyberv1.AgentJobOutcomeSuccess},
	}

	handler, _ := buildAgentHandler(t, agent)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/agents/chewie", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Jobs        []api.AgentJobRequest `json:"jobs"`
		LastJobRuns []struct {
			JobName string `json:"jobName"`
			Outcome string `json:"outcome"`
		} `json:"lastJobRuns"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Jobs) != 1 || body.Jobs[0].Name != "morning" {
		t.Errorf("jobs: %+v", body.Jobs)
	}
	if len(body.LastJobRuns) != 1 {
		t.Fatalf("lastJobRuns: want 1 entry, got %d", len(body.LastJobRuns))
	}
	if body.LastJobRuns[0].Outcome != "success" {
		t.Errorf("lastJobRuns[0].Outcome: got %q, want success (later entry should win)", body.LastJobRuns[0].Outcome)
	}
}

// TestAgentAPI_RunJob_RejectsUnknown confirms POST /jobs/{name}/run returns
// 404 when the agent doesn't declare the named job.
func TestAgentAPI_RunJob_RejectsUnknown(t *testing.T) {
	agent := sampleAgentCRD("chewie")
	handler, _ := buildAgentHandler(t, agent)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/chewie/jobs/ghost/run", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body := readAll(t, resp)
		t.Errorf("status: got %d, want 404 (%s)", resp.StatusCode, body)
	}
}

// TestAgentAPI_RunJob_ServiceUnavailableWithoutClientset confirms 503 when
// RestConfig/Clientset aren't wired — matches the exec endpoint's behavior.
func TestAgentAPI_RunJob_ServiceUnavailableWithoutClientset(t *testing.T) {
	agent := sampleAgentCRD("chewie")
	agent.Spec.Jobs = []kyberv1.AgentJob{{Name: "morning", Schedule: "0 9 * * *", Prompt: "hi"}}
	handler, _ := buildAgentHandler(t, agent)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/chewie/jobs/morning/run", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
}

// TestAgentAPI_RunJob_MethodNotAllowed guards against accidental GET handling.
func TestAgentAPI_RunJob_MethodNotAllowed(t *testing.T) {
	agent := sampleAgentCRD("chewie")
	agent.Spec.Jobs = []kyberv1.AgentJob{{Name: "morning", Schedule: "0 9 * * *", Prompt: "hi"}}
	handler, _ := buildAgentHandler(t, agent)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/agents/chewie/jobs/morning/run", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", resp.StatusCode)
	}
}

// readAll drains the body as a string for failure-path test output.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// Prevent unused import when resource{} isn't referenced by this file.
var _ = resource.Quantity{}
