package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// buildExecHandler creates a test handler. RestConfig and Clientset are intentionally nil
// so that exec pre-checks (agent lookup, pod name) are exercisable without a real cluster.
func buildExecHandler(t *testing.T, objs ...runtime.Object) http.Handler {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	s := &api.Server{
		K8sClient:     fakeClient,
		MessageBuffer: messagebuffer.NewMemoryBuffer(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		// Clientset and RestConfig intentionally nil — tests error paths only
	}
	return s.BuildHandler()
}

// sampleAgentForExec returns an Agent CRD with a pod name set.
func sampleAgentForExec(name, podName string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "worker-1",
			Runtime: "claude-code",
			Model:   "claude-sonnet-4",
			Scaling: kyberv1.AgentScalingWarm,
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
		Status: kyberv1.AgentStatus{
			PodName: podName,
			Phase:   kyberv1.AgentPhaseRunning,
		},
	}
}

// TestExecStreamParams_IncludesContainer pins kyber#264 — pre-fix the
// exec request was built without a container param, and on the post-#248
// multi-container agent pod kubelet picked an unspecified container
// (often the distroless sidecar) so Activity + Shell tabs silently
// broke. Container must always be explicit.
func TestExecStreamParams_IncludesContainer(t *testing.T) {
	got := api.ExecStreamParamsForTest("agent")
	if got["container"] != "agent" {
		t.Errorf("container param: got %q, want %q", got["container"], "agent")
	}
	for _, key := range []string{"stdin", "stdout", "stderr", "tty"} {
		if got[key] != "true" {
			t.Errorf("%s param: got %q, want %q", key, got[key], "true")
		}
	}
}

// TestExec_AgentNotFound verifies 404 when the agent CRD does not exist.
func TestExec_AgentNotFound(t *testing.T) {
	h := buildExecHandler(t)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/missing/exec", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestExec_AgentNoPod verifies 503 when agent exists but RestConfig/Clientset is nil.
// The handler now derives the pod name from the agent name — it no longer checks
// Status.PodName — so the first error encountered is the nil restConfig guard.
func TestExec_AgentNoPod(t *testing.T) {
	agent := sampleAgentCRD("dave") // Status.PodName is empty — handler ignores it
	h := buildExecHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/exec", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// With the fix the handler derives "agent-dave" and hits the nil restConfig/clientset guard.
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 (nil restConfig) for agent with no pod, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestExec_NoRestConfig verifies 503 when RestConfig is nil.
func TestExec_NoRestConfig(t *testing.T) {
	agent := sampleAgentForExec("dave", "dave-pod-abc")
	h := buildExecHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/exec", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when restConfig nil, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestExec_NoAuth verifies 401 when auth header is missing.
func TestExec_NoAuth(t *testing.T) {
	h := buildExecHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dave/exec", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestExec_InvalidName verifies 400 for invalid agent names in the exec path.
func TestExec_InvalidName(t *testing.T) {
	h := buildExecHandler(t)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/INVALID!/exec", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachineExec_MachineNotFound verifies 404 when the machine CRD does not exist.
func TestMachineExec_MachineNotFound(t *testing.T) {
	h := buildExecHandler(t)
	req := authedRequest(t, http.MethodGet, "/api/v1/machines/missing/exec", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachineExec_NoRestConfig verifies 503 when RestConfig is nil.
func TestMachineExec_NoRestConfig(t *testing.T) {
	machine := sampleMachineCRD("node-1")
	h := buildExecHandler(t, machine)
	req := authedRequest(t, http.MethodGet, "/api/v1/machines/node-1/exec", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when restConfig nil, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMachineExec_InvalidMode verifies an unknown mode returns 400 on the machine path.
func TestMachineExec_InvalidMode(t *testing.T) {
	machine := sampleMachineCRD("node-1")
	h := buildExecHandler(t, machine)
	req := authedRequest(t, http.MethodGet, "/api/v1/machines/node-1/exec?mode=bogus", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400 for unknown mode on machine path, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestExec_WebSocketUpgrade verifies that a WebSocket upgrade is attempted when
// the agent has a pod and config is available. We use a nil RestConfig so the
// handler returns 503 before upgrading — this confirms the pre-upgrade checks work.
// Full WebSocket exec is exercised in D3 e2e tests with a real k3d cluster.
func TestExec_WebSocketPrecheck(t *testing.T) {
	// With nil RestConfig, 503 is returned *before* the WebSocket upgrade.
	// This verifies the handler correctly pre-checks before doing the upgrade.
	agent := sampleAgentForExec("dave", "dave-pod-xyz")
	h := buildExecHandler(t, agent)

	u := "ws" + strings.TrimPrefix("http://example.com", "http") + "/api/v1/agents/dave/exec"
	_ = u // Not used — we use plain HTTP to confirm pre-upgrade 503.

	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/exec", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// 503 expected because RestConfig is nil — not a WebSocket upgrade failure.
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestParseExecCommand_ModeAttach verifies ?mode=attach maps to the read-only tmux attach.
func TestParseExecCommand_ModeAttach(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?mode=attach", nil)
	got := api.ParseExecCommandForTest(req)
	want := []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd", "--", "sudo", "-iu", "kyber", "tmux", "-u", "attach", "-t", "agent", "-r"}
	if !equalStringSlice(got, want) {
		t.Errorf("mode=attach: got %v, want %v", got, want)
	}
}

// TestParseExecCommand_ModeShell verifies ?mode=shell maps to a login bash.
func TestParseExecCommand_ModeShell(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?mode=shell", nil)
	got := api.ParseExecCommandForTest(req)
	want := []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd", "--", "/bin/bash", "-l"}
	if !equalStringSlice(got, want) {
		t.Errorf("mode=shell: got %v, want %v", got, want)
	}
}

// TestParseExecCommand_ModeHistory verifies ?mode=history maps to a one-shot
// tmux capture-pane of the agent session (#122 V2).
func TestParseExecCommand_ModeHistory(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?mode=history", nil)
	got := api.ParseExecCommandForTest(req)
	want := []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd", "--", "sudo", "-iu", "kyber", "tmux", "capture-pane", "-peJ", "-S", "-10000", "-t", "agent"}
	if !equalStringSlice(got, want) {
		t.Errorf("mode=history: got %v, want %v", got, want)
	}
}

func TestParseExecCommand_ModeDeviceAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/dave/exec?mode=device-auth", nil)
	got := api.ParseExecCommandForTest(req)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "tmux -u attach -t auth -r") {
		t.Fatalf("device-auth command = %q, want read-only auth-session attach", joined)
	}
}

// TestParseExecCommand_DefaultStillBash verifies no params still defaults to /bin/bash.
func TestParseExecCommand_DefaultStillBash(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	got := api.ParseExecCommandForTest(req)
	want := []string{"/bin/bash"}
	if !equalStringSlice(got, want) {
		t.Errorf("default: got %v, want %v", got, want)
	}
}

// TestParseExecCommand_CmdOverridesMode verifies an explicit ?cmd= wins over ?mode=.
func TestParseExecCommand_CmdOverridesMode(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?mode=attach&cmd=ls", nil)
	got := api.ParseExecCommandForTest(req)
	want := []string{"ls"}
	if !equalStringSlice(got, want) {
		t.Errorf("cmd-overrides-mode: got %v, want %v", got, want)
	}
}

// TestExec_InvalidMode verifies an unknown mode returns 400.
func TestExec_InvalidMode(t *testing.T) {
	agent := sampleAgentForExec("dave", "dave-pod")
	h := buildExecHandler(t, agent)
	req := authedRequest(t, http.MethodGet, "/api/v1/agents/dave/exec?mode=bogus", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400 for unknown mode, got %d: %s", rr.Code, rr.Body.String())
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
