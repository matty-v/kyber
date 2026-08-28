package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

type fakeRuntimeRepairRunner struct {
	output string
	err    error
	calls  int
	plan   api.RuntimeRepairPlan
	after  func()
}

func (f *fakeRuntimeRepairRunner) Run(_ context.Context, _ *kyberv1.Agent, plan api.RuntimeRepairPlan) (string, error) {
	f.calls++
	f.plan = plan
	if f.after != nil {
		f.after()
	}
	return f.output, f.err
}

func brokenRuntimeAgent(name string) *kyberv1.Agent {
	agent := sampleAgentCRD(name)
	agent.UID = types.UID(name + "-uid")
	agent.Spec.Runtime = "claude-code"
	agent.Spec.RuntimeVersion = "2.1.250"
	agent.Status.Phase = kyberv1.AgentPhaseBrokenRuntime
	agent.Status.Runtime = kyberv1.AgentRuntimeStatus{Runtime: "claude-code", Usable: boolPtr(false), ProbeMessage: "syntax error"}
	return agent
}

func boolPtr(v bool) *bool { return &v }

func repairTestServer(t *testing.T, agent *kyberv1.Agent, runner api.RuntimeRepairRunner) *api.Server {
	t.Helper()
	s := newTestPublicServer(t, testAPIKey)
	if err := s.K8sClient.Create(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	s.RuntimeRepairPlans = map[string]api.RuntimeRepairPlan{
		"claude-code": {
			Image:          "kyber/claude-code:test",
			PackageName:    "@anthropic-ai/claude-code",
			BinaryName:     "claude",
			PackagePath:    "/usr/lib/node_modules/@anthropic-ai/claude-code",
			ExecutablePath: "/usr/bin/claude",
		},
	}
	s.RuntimeRepairRunner = runner
	return s
}

func postRepair(t *testing.T, s *api.Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := scopedRequest(http.MethodPost, "/api/v1/agents/"+name+"/repair-runtime", testAPIKey)
	rr := httptest.NewRecorder()
	buildTestHandler(s).ServeHTTP(rr, req)
	return rr
}

func TestRepairRuntimeSuccessRequestsRestart(t *testing.T) {
	runner := &fakeRuntimeRepairRunner{output: "verified claude 2.1.250"}
	s := repairTestServer(t, brokenRuntimeAgent("repair-success"), runner)
	rr := postRepair(t, s, "repair-success")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if runner.calls != 1 || runner.plan.Version != "2.1.250" {
		t.Fatalf("runner calls=%d plan=%+v", runner.calls, runner.plan)
	}
	stored := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: "repair-success", Namespace: "kyber-system"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("desiredPhase=%q, want Restarting", stored.Spec.DesiredPhase)
	}
}

func TestRepairRuntimeWrongPhaseDoesNotRun(t *testing.T) {
	agent := brokenRuntimeAgent("repair-running")
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	runner := &fakeRuntimeRepairRunner{}
	s := repairTestServer(t, agent, runner)
	rr := postRepair(t, s, agent.Name)
	if rr.Code != http.StatusConflict || runner.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, runner.calls, rr.Body.String())
	}
}

func TestRepairRuntimeFailureRetainsBrokenState(t *testing.T) {
	agent := brokenRuntimeAgent("repair-fails")
	runner := &fakeRuntimeRepairRunner{err: errors.New("npm registry unavailable")}
	s := repairTestServer(t, agent, runner)
	rr := postRepair(t, s, agent.Name)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "remains in BrokenRuntime") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	stored := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: agent.Name, Namespace: "kyber-system"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Phase != kyberv1.AgentPhaseBrokenRuntime || stored.Spec.DesiredPhase == kyberv1.AgentPhaseRestarting {
		t.Fatalf("agent changed after failed repair: phase=%s desired=%s", stored.Status.Phase, stored.Spec.DesiredPhase)
	}
}

func TestRepairRuntimeConflict(t *testing.T) {
	agent := brokenRuntimeAgent("repair-conflict")
	runner := &fakeRuntimeRepairRunner{err: api.ErrRuntimeRepairInProgress}
	s := repairTestServer(t, agent, runner)
	rr := postRepair(t, s, agent.Name)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "repair_in_progress") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepairRuntimeDoesNotOverwriteConcurrentStop(t *testing.T) {
	agent := brokenRuntimeAgent("repair-stopped")
	runner := &fakeRuntimeRepairRunner{output: "verified claude 2.1.250"}
	s := repairTestServer(t, agent, runner)
	runner.after = func() {
		current := &kyberv1.Agent{}
		key := types.NamespacedName{Name: agent.Name, Namespace: "kyber-system"}
		if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
			t.Fatal(err)
		}
		current.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
		if err := s.K8sClient.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}

	rr := postRepair(t, s, agent.Name)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "agent_changed") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	stored := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: agent.Name, Namespace: "kyber-system"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DesiredPhase != kyberv1.AgentPhaseStopped {
		t.Fatalf("desiredPhase=%q, want Stopped", stored.Spec.DesiredPhase)
	}
}

func TestRepairRuntimeDoesNotRestartRecreatedAgent(t *testing.T) {
	agent := brokenRuntimeAgent("repair-recreated")
	runner := &fakeRuntimeRepairRunner{output: "verified claude 2.1.250"}
	s := repairTestServer(t, agent, runner)
	runner.after = func() {
		current := &kyberv1.Agent{}
		key := types.NamespacedName{Name: agent.Name, Namespace: "kyber-system"}
		if err := s.K8sClient.Get(context.Background(), key, current); err != nil {
			t.Fatal(err)
		}
		if err := s.K8sClient.Delete(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		recreated := brokenRuntimeAgent(agent.Name)
		recreated.UID = types.UID("replacement-uid")
		if err := s.K8sClient.Create(context.Background(), recreated); err != nil {
			t.Fatal(err)
		}
	}

	rr := postRepair(t, s, agent.Name)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "agent_changed") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	stored := &kyberv1.Agent{}
	if err := s.K8sClient.Get(context.Background(), types.NamespacedName{Name: agent.Name, Namespace: "kyber-system"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.UID != types.UID("replacement-uid") || stored.Spec.DesiredPhase == kyberv1.AgentPhaseRestarting {
		t.Fatalf("recreated agent was changed: uid=%q desired=%q", stored.UID, stored.Spec.DesiredPhase)
	}
}
