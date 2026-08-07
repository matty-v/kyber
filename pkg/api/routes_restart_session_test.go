package api_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// newRestartSessionServer builds an api.Server for restart-session tests.
// RestartSessionCommands is populated per-test so the 501 path can be
// exercised by omitting a runtime. Clientset / RestConfig are intentionally
// nil — the handler's 503 branch lets us assert the guard ordering without
// needing a real k8s exec stack.
func newRestartSessionServer(t *testing.T, cmds map[string][]string, objs ...runtime.Object) *api.Server {
	t.Helper()
	scheme := mustNewScheme(t)
	all := append([]runtime.Object{defaultMachine()}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(all...).Build()
	return &api.Server{
		K8sClient:              fakeClient,
		MessageBuffer:          messagebuffer.NewMemoryBuffer(),
		APIKey:                 testAPIKey,
		Namespace:              "kyber-system",
		ValidRuntimes:          map[string]bool{"claude-code": true, "openclaw": true},
		RestartSessionCommands: cmds,
	}
}

// TestRestartSession_501UnknownRuntime verifies a runtime with no command
// registered returns 501 (the adapter-hasn't-shipped-yet case).
func TestRestartSession_501UnknownRuntime(t *testing.T) {
	api.ResetRestartSessionCooldown()
	agent := sampleAgentCRD("chewie")
	agent.Spec.Runtime = "openclaw" // not registered
	agent.Status.Phase = kyberv1.AgentPhaseRunning

	// Custom server: RestartSessionCommands map has only claude-code.
	srv := newRestartSessionServer(t, map[string][]string{
		"claude-code": {"nsenter", "--target", "1", "--", "/bin/bash", "/persist/last-claude-launch.sh"},
	}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/chewie/restart-session", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status: got %d, want 501", resp.StatusCode)
	}
}

// TestRestartSession_409NotRunning verifies the phase guard fires for a
// stopped/suspended/any-non-Running agent.
func TestRestartSession_409NotRunning(t *testing.T) {
	api.ResetRestartSessionCooldown()
	agent := sampleAgentCRD("chewie")
	agent.Spec.Runtime = "claude-code"
	agent.Status.Phase = kyberv1.AgentPhaseStopped

	srv := newRestartSessionServer(t, map[string][]string{
		"claude-code": {"/bin/bash", "/persist/last-claude-launch.sh"},
	}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/chewie/restart-session", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status: got %d, want 409", resp.StatusCode)
	}
}

// TestRestartSession_429Cooldown verifies the second call inside the
// cooldown window returns 429 with a Retry-After header. Uses the
// 503-on-no-clientset branch to keep the "happy path" from actually
// exec'ing — but the restart is recorded BEFORE the exec, so the first
// call still stamps the cooldown map.
func TestRestartSession_429Cooldown(t *testing.T) {
	api.ResetRestartSessionCooldown()
	agent := sampleAgentCRD("chewie")
	agent.Spec.Runtime = "claude-code"
	agent.Status.Phase = kyberv1.AgentPhaseRunning

	srv := newRestartSessionServer(t, map[string][]string{
		"claude-code": {"/bin/bash", "/persist/last-claude-launch.sh"},
	}, agent)
	// Intentionally nil clientset so the first call reaches the 503 branch
	// without running an exec — but *after* recordRestartSession stamps
	// the cooldown. Actually — re-check implementation order: the 503
	// check runs BEFORE recordRestartSession, so the first call wouldn't
	// stamp. Re-order the test to use a real exec path would be ideal; for
	// a unit test, directly stamp the map instead.
	// The 429 branch fires purely on the cooldown map, so we call
	// recordRestartSession directly through the handler's first path.
	// Since we can't easily run a happy-path here without a clientset,
	// stamp the cooldown by calling handleRestartSession once — which
	// will go all the way to the 503 branch, NOT stamping. So we'd never
	// hit the 429 branch from a second real call.
	//
	// Simpler approach: call the exported ResetRestartSessionCooldown +
	// then pre-populate via a helper. Don't have that helper — add one.
	api.StampRestartSessionForTest("chewie")

	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/chewie/restart-session", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status: got %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
}

// TestRestartSession_404Unknown verifies an agent that doesn't exist returns 404.
func TestRestartSession_404Unknown(t *testing.T) {
	api.ResetRestartSessionCooldown()
	srv := newRestartSessionServer(t, map[string][]string{
		"claude-code": {"/bin/bash", "/persist/last-claude-launch.sh"},
	})
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/ghost/restart-session", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestRestartSession_503NoClientset verifies the 503 branch when RestConfig
// or Clientset isn't wired.
func TestRestartSession_503NoClientset(t *testing.T) {
	api.ResetRestartSessionCooldown()
	agent := sampleAgentCRD("chewie")
	agent.Spec.Runtime = "claude-code"
	agent.Status.Phase = kyberv1.AgentPhaseRunning

	srv := newRestartSessionServer(t, map[string][]string{
		"claude-code": {"/bin/bash", "/persist/last-claude-launch.sh"},
	}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/chewie/restart-session", nil)
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

// TestRestartSession_CooldownRaceSerializes proves the cooldown check +
// stamp are atomic: N parallel POSTs for the same agent land with exactly
// one 503 (the claim-winner, which then fails on the missing clientset)
// and the rest in 429 (cooldown). Regression guard for the earlier
// shouldThrottle+record pair, which let multiple requests slip through
// the window between the two calls.
func TestRestartSession_CooldownRaceSerializes(t *testing.T) {
	api.ResetRestartSessionCooldown()
	agent := sampleAgentCRD("chewie")
	agent.Spec.Runtime = "claude-code"
	agent.Status.Phase = kyberv1.AgentPhaseRunning

	srv := newRestartSessionServer(t, map[string][]string{
		"claude-code": {"/bin/bash", "/persist/last-claude-launch.sh"},
	}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	const parallel = 32
	var claimWinners, throttled atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost,
				ts.URL+"/api/v1/agents/chewie/restart-session", nil)
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("POST: %v", err)
				return
			}
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusServiceUnavailable:
				// The claim winner — reached the clientset-missing branch.
				claimWinners.Add(1)
			case http.StatusTooManyRequests:
				throttled.Add(1)
			default:
				t.Errorf("unexpected status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if got := claimWinners.Load(); got != 1 {
		t.Errorf("claim winners: got %d, want 1 (cooldown not atomic — race present)", got)
	}
	if got := throttled.Load(); got != parallel-1 {
		t.Errorf("throttled: got %d, want %d", got, parallel-1)
	}
}

// TestRestartSession_405OnGet guards the method check.
func TestRestartSession_405OnGet(t *testing.T) {
	api.ResetRestartSessionCooldown()
	agent := sampleAgentCRD("chewie")
	agent.Spec.Runtime = "claude-code"
	agent.Status.Phase = kyberv1.AgentPhaseRunning

	srv := newRestartSessionServer(t, map[string][]string{
		"claude-code": {"/bin/bash", "/persist/last-claude-launch.sh"},
	}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/agents/chewie/restart-session", nil)
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
