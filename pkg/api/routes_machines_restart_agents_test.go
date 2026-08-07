package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// buildRestartAgentsHandler mirrors buildMachineHandler but returns the
// underlying client and recorder so tests can assert side effects.
func buildRestartAgentsHandler(t *testing.T, scheme *runtime.Scheme, objs ...runtime.Object) (http.Handler, client.Client, *record.FakeRecorder) {
	t.Helper()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	rec := record.NewFakeRecorder(16)
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		Recorder:      rec,
	}
	return s.BuildHandler(), fakeClient, rec
}

func agentOnMachine(name, machine string, phase kyberv1.AgentPhase) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: machine,
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
		},
		Status: kyberv1.AgentStatus{Phase: phase},
	}
}

// TestRestartMachineAgents_HappyPath verifies 3 eligible agents on the target
// machine each get desiredPhase=Restarting and are listed in the response.
func TestRestartMachineAgents_HappyPath(t *testing.T) {
	scheme := mustNewScheme(t)
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50},
		Status:     kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseReady},
	}
	chewie := agentOnMachine("chewie", "worker-1", kyberv1.AgentPhaseRunning)
	han := agentOnMachine("han", "worker-1", kyberv1.AgentPhaseStarting)
	lando := agentOnMachine("lando", "worker-1", kyberv1.AgentPhaseFailed)

	h, c, rec := buildRestartAgentsHandler(t, scheme, m, chewie, han, lando)

	req := authedRequest(t, http.MethodPost, "/api/v1/machines/worker-1/restart-agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.RestartMachineAgentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 3 {
		t.Errorf("Count: got %d, want 3", resp.Count)
	}
	got := append([]string(nil), resp.Restarted...)
	sort.Strings(got)
	want := []string{"chewie", "han", "lando"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Restarted: got %v, want %v", got, want)
	}
	if len(resp.Skipped) != 0 {
		t.Errorf("Skipped: got %v, want empty", resp.Skipped)
	}

	// Every restarted agent should have desiredPhase=Restarting.
	for _, name := range want {
		var got kyberv1.Agent
		if err := c.Get(req.Context(), client.ObjectKey{Name: name, Namespace: "kyber-system"}, &got); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if got.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
			t.Errorf("%s desiredPhase: got %q, want %q", name, got.Spec.DesiredPhase, kyberv1.AgentPhaseRestarting)
		}
	}

	// Exactly one MachineAgentsRestarted event should have been emitted.
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "MachineAgentsRestarted") {
			t.Errorf("event reason: got %q, want containing MachineAgentsRestarted", ev)
		}
		if !strings.Contains(ev, "3 agent") {
			t.Errorf("event body should mention restarted count; got %q", ev)
		}
	default:
		t.Errorf("expected a MachineAgentsRestarted event, got none")
	}
}

// TestRestartMachineAgents_SkipsIneligiblePhases verifies that Suspended,
// Stopped, Draining, and WaitingForMachine agents are NOT patched and are
// reported in the Skipped list.
func TestRestartMachineAgents_SkipsIneligiblePhases(t *testing.T) {
	scheme := mustNewScheme(t)
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50},
	}
	running := agentOnMachine("chewie", "worker-1", kyberv1.AgentPhaseRunning)
	suspended := agentOnMachine("r2d2", "worker-1", kyberv1.AgentPhaseSuspended)
	stopped := agentOnMachine("c3po", "worker-1", kyberv1.AgentPhaseStopped)
	draining := agentOnMachine("bb8", "worker-1", kyberv1.AgentPhaseDraining)
	waiting := agentOnMachine("d0", "worker-1", kyberv1.AgentPhaseWaitingForMachine)

	h, c, _ := buildRestartAgentsHandler(t, scheme, m, running, suspended, stopped, draining, waiting)

	req := authedRequest(t, http.MethodPost, "/api/v1/machines/worker-1/restart-agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.RestartMachineAgentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 || len(resp.Restarted) != 1 || resp.Restarted[0] != "chewie" {
		t.Errorf("Restarted: got %v (count=%d), want [chewie] (count=1)", resp.Restarted, resp.Count)
	}
	if len(resp.Skipped) != 4 {
		t.Fatalf("Skipped: got %d entries, want 4: %+v", len(resp.Skipped), resp.Skipped)
	}
	reasons := map[string]string{}
	for _, sk := range resp.Skipped {
		reasons[sk.Name] = sk.Reason
	}
	expectedReasons := map[string]string{
		"r2d2": "Suspended",
		"c3po": "Stopped",
		"bb8":  "Draining",
		"d0":   "WaitingForMachine",
	}
	for name, want := range expectedReasons {
		if got := reasons[name]; got != want {
			t.Errorf("skipped[%s].reason: got %q, want %q", name, got, want)
		}
	}

	// Ineligible agents must NOT have their desiredPhase mutated.
	for _, name := range []string{"r2d2", "c3po", "bb8", "d0"} {
		var got kyberv1.Agent
		if err := c.Get(req.Context(), client.ObjectKey{Name: name, Namespace: "kyber-system"}, &got); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if got.Spec.DesiredPhase == kyberv1.AgentPhaseRestarting {
			t.Errorf("%s: desiredPhase was patched to Restarting — should have been skipped", name)
		}
	}
}

// TestRestartMachineAgents_IgnoresOtherMachines verifies that agents on other
// machines are neither restarted nor reported in the skipped list.
func TestRestartMachineAgents_IgnoresOtherMachines(t *testing.T) {
	scheme := mustNewScheme(t)
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50},
	}
	target := agentOnMachine("chewie", "worker-1", kyberv1.AgentPhaseRunning)
	other := agentOnMachine("dave", "worker-2", kyberv1.AgentPhaseRunning)

	h, c, _ := buildRestartAgentsHandler(t, scheme, m, target, other)

	req := authedRequest(t, http.MethodPost, "/api/v1/machines/worker-1/restart-agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.RestartMachineAgentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 || len(resp.Restarted) != 1 || resp.Restarted[0] != "chewie" {
		t.Errorf("Restarted: got %v, want [chewie]", resp.Restarted)
	}
	if len(resp.Skipped) != 0 {
		t.Errorf("Skipped should not include agents on other machines: %+v", resp.Skipped)
	}

	var daveGot kyberv1.Agent
	if err := c.Get(req.Context(), client.ObjectKey{Name: "dave", Namespace: "kyber-system"}, &daveGot); err != nil {
		t.Fatalf("get dave: %v", err)
	}
	if daveGot.Spec.DesiredPhase == kyberv1.AgentPhaseRestarting {
		t.Errorf("dave (on worker-2) was incorrectly restarted by a worker-1 action")
	}
}

// TestRestartMachineAgents_NotFound verifies 404 for a non-existent machine.
func TestRestartMachineAgents_NotFound(t *testing.T) {
	scheme := mustNewScheme(t)
	h, _, _ := buildRestartAgentsHandler(t, scheme)
	req := authedRequest(t, http.MethodPost, "/api/v1/machines/ghost/restart-agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRestartMachineAgents_NoAgents verifies that a machine with zero agents
// returns 200 and an empty restarted list — not an error.
func TestRestartMachineAgents_NoAgents(t *testing.T) {
	scheme := mustNewScheme(t)
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "kyber-system"},
		Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50},
	}
	h, _, _ := buildRestartAgentsHandler(t, scheme, m)

	req := authedRequest(t, http.MethodPost, "/api/v1/machines/empty/restart-agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.RestartMachineAgentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 0 || len(resp.Restarted) != 0 || len(resp.Skipped) != 0 {
		t.Errorf("expected empty response for machine with no agents; got %+v", resp)
	}
}

// TestRestartMachineAgents_MethodNotAllowed verifies GET returns 405.
func TestRestartMachineAgents_MethodNotAllowed(t *testing.T) {
	scheme := mustNewScheme(t)
	m := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderGCE, MachineType: "n2-standard-4", Zone: "us-central1-a", DiskSizeGb: 50},
	}
	h, _, _ := buildRestartAgentsHandler(t, scheme, m)

	req := authedRequest(t, http.MethodGet, "/api/v1/machines/worker-1/restart-agents", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rr.Code)
	}
}

