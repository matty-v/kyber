package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/podtoken"
)

func TestAgentGoalLifecycleAndStaleRevision(t *testing.T) {
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"}}
	fakeClient := fake.NewClientBuilder().WithScheme(statusEventTestScheme(t)).WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(path, body string) (*http.Response, map[string]string) {
		t.Helper()
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		_ = resp.Body.Close()
		return resp, decoded
	}

	first := "2026-09-16T12:00:00.123456789Z"
	resp, goal := post("/internal/agents/alice/goal-start", fmt.Sprintf(`{"acceptedAt":%q}`, first))
	if resp.StatusCode != http.StatusOK || goal["summary"] != "Working on a new request" || goal["source"] != "platform" {
		t.Fatalf("start status=%d goal=%v", resp.StatusCode, goal)
	}
	firstRevision := goal["acceptedAt"]
	resp, goal = post("/internal/agents/alice/goal", fmt.Sprintf(`{"acceptedAt":%q,"summary":"  Implement\nagent   goals  "}`, firstRevision))
	if resp.StatusCode != http.StatusOK || goal["summary"] != "Implement agent goals" || goal["source"] != "agent" {
		t.Fatalf("refine status=%d goal=%v", resp.StatusCode, goal)
	}
	// A control-plane retry carries the original nanosecond timestamp. It must
	// compare equal to the persisted microsecond revision and preserve the
	// semantic refinement rather than resetting the fallback.
	resp, goal = post("/internal/agents/alice/goal-start", fmt.Sprintf(`{"acceptedAt":%q}`, first))
	if resp.StatusCode != http.StatusOK || goal["summary"] != "Implement agent goals" || goal["source"] != "agent" {
		t.Fatalf("duplicate start status=%d goal=%v", resp.StatusCode, goal)
	}

	second := "2026-09-16T12:01:00Z"
	resp, _ = post("/internal/agents/alice/goal-start", fmt.Sprintf(`{"acceptedAt":%q}`, second))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second start status=%d", resp.StatusCode)
	}
	resp, _ = post("/internal/agents/alice/goal", fmt.Sprintf(`{"acceptedAt":%q,"summary":"late write"}`, firstRevision))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale refine status=%d, want 409", resp.StatusCode)
	}

	got := &kyberv1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "kyber-system", Name: "alice"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Goal == nil || got.Status.Goal.AcceptedAt.UTC().Format("2006-01-02T15:04:05Z07:00") != second {
		t.Fatalf("stored goal=%+v", got.Status.Goal)
	}
}

func TestAgentGoalPersistsAcrossIdleAndServerRestart(t *testing.T) {
	accepted := metav1.NewMicroTime(metav1.Now().Time)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
		Status: kyberv1.AgentStatus{
			Activity: &kyberv1.ActivityStatus{State: "idle"},
			Goal: &kyberv1.AgentGoalStatus{
				Summary: "Review MAT-62", Source: "agent", AcceptedAt: accepted, UpdatedAt: accepted,
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(statusEventTestScheme(t)).WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).Build()

	// Constructing a new InternalServer models a control-plane restart while
	// retaining the Kubernetes Agent status that is the source of truth.
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	req := httptest.NewRequest(http.MethodGet, "/internal/agents/alice/goal", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var goal map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&goal); err != nil {
		t.Fatal(err)
	}
	if goal["summary"] != "Review MAT-62" || goal["source"] != "agent" {
		t.Fatalf("goal after idle/restart=%v", goal)
	}
}

func TestAgentGoalInternalAPIIsSelfScoped(t *testing.T) {
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"}}
	fakeClient := fake.NewClientBuilder().WithScheme(statusEventTestScheme(t)).WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).Build()
	srv := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"),
		api.WithInternalAuth(api.NewHMACInternalAuthenticator(testSigningKey), false),
	)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"acceptedAt":"2026-09-16T12:00:00Z"}`
	resp := do(t, ts, http.MethodPost, "/internal/agents/alice/goal-start", podtoken.Sign("other", testSigningKey), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-agent status=%d, want 403", resp.StatusCode)
	}
}

func TestAgentGoalRejectsInvalidSummary(t *testing.T) {
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"}}
	fakeClient := fake.NewClientBuilder().WithScheme(statusEventTestScheme(t)).WithObjects(agent).
		WithStatusSubresource(&kyberv1.Agent{}).Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	at := "2026-09-16T12:00:00Z"
	startResp, err := http.Post(ts.URL+"/internal/agents/alice/goal-start", "application/json", strings.NewReader(fmt.Sprintf(`{"acceptedAt":%q}`, at)))
	if err != nil {
		t.Fatal(err)
	}
	_ = startResp.Body.Close()
	tooLong := strings.Repeat("x", 121)
	resp, err := http.Post(ts.URL+"/internal/agents/alice/goal", "application/json", strings.NewReader(fmt.Sprintf(`{"acceptedAt":%q,"summary":%q}`, at, tooLong)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}
