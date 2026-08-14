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

// compactCmd is a stand-in argv. The handler never inspects its contents —
// only whether the runtime has one — so the value just has to be non-empty.
var compactCmd = []string{"/usr/local/bin/kyber-compact-session", "/compact"}

// newCompactSessionServer builds an api.Server for compact-session tests.
// Same shape as newRestartSessionServer: Clientset/RestConfig stay nil so
// the 503 branch stands in for "the guards all passed", letting every guard
// be asserted without a real exec stack.
func newCompactSessionServer(t *testing.T, cmds map[string][]string, objs ...runtime.Object) *api.Server {
	t.Helper()
	scheme := mustNewScheme(t)
	all := append([]runtime.Object{defaultMachine()}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(all...).Build()
	return &api.Server{
		K8sClient:              fakeClient,
		MessageBuffer:          messagebuffer.NewMemoryBuffer(),
		APIKey:                 testAPIKey,
		Namespace:              "kyber-system",
		ValidRuntimes:          map[string]bool{"claude-code": true, "codex": true, "openclaw": true},
		CompactSessionCommands: cmds,
	}
}

// runningCompactAgent returns a Running agent on the given runtime.
func runningCompactAgent(name, rt string) *kyberv1.Agent {
	agent := sampleAgentCRD(name)
	agent.Spec.Runtime = rt
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	return agent
}

// postCompact issues the authenticated POST and returns the response.
func postCompact(t *testing.T, ts *httptest.Server, name string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/"+name+"/compact-session", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// TestCompactSession_501UnsupportedRuntime: a runtime with no compaction
// command registered must answer 501, not 500 — "this runtime can't do it"
// is a normal answer, and the operator's next move (restart-session) is
// different from what they'd do about a server fault.
func TestCompactSession_501UnsupportedRuntime(t *testing.T) {
	api.ResetCompactSessionCooldown()
	agent := runningCompactAgent("chewie", "openclaw") // not in the map

	srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	resp := postCompact(t, ts, "chewie")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status: got %d, want 501", resp.StatusCode)
	}
}

// TestCompactSession_CodexSupported guards against a regression where only
// claude-code gets wired up. Both shipped runtimes must reach the same
// branch — here the 503, which is past every runtime-specific guard.
func TestCompactSession_CodexSupported(t *testing.T) {
	api.ResetCompactSessionCooldown()
	agent := runningCompactAgent("hk47", "codex")

	srv := newCompactSessionServer(t, map[string][]string{
		"claude-code": compactCmd,
		"codex":       compactCmd,
	}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	resp := postCompact(t, ts, "hk47")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503 (codex should pass the 501 guard)", resp.StatusCode)
	}
}

// TestCompactSession_EmptyCommandIs501: an entry present but empty must be
// treated the same as absent. A `map[string][]string{"codex": {}}` is what a
// half-wired adapter produces, and exec'ing an empty argv would fail much
// later and much less clearly.
func TestCompactSession_EmptyCommandIs501(t *testing.T) {
	api.ResetCompactSessionCooldown()
	agent := runningCompactAgent("chewie", "codex")

	srv := newCompactSessionServer(t, map[string][]string{"codex": {}}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	resp := postCompact(t, ts, "chewie")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status: got %d, want 501", resp.StatusCode)
	}
}

// TestCompactSession_409NotRunning: no live session means nothing to
// compact. Asserted across every non-Running phase rather than just one, so
// a future phase can't silently fall through to an exec against a dead pod.
func TestCompactSession_409NotRunning(t *testing.T) {
	phases := []kyberv1.AgentPhase{
		kyberv1.AgentPhaseStopped,
		kyberv1.AgentPhaseSuspended,
		kyberv1.AgentPhaseStarting,
		kyberv1.AgentPhaseNeedsAuth,
		// The tempting one: an OOM-killed agent looks like the perfect
		// candidate for "shrink the context", but its container is dead —
		// there is no tmux session to paste into. The fix is more memory
		// plus a restart, not compaction.
		kyberv1.AgentPhaseMemoryExhausted,
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			api.ResetCompactSessionCooldown()
			agent := sampleAgentCRD("chewie")
			agent.Spec.Runtime = "claude-code"
			agent.Status.Phase = phase

			srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd}, agent)
			ts := httptest.NewServer(srv.BuildHandler())
			defer ts.Close()

			resp := postCompact(t, ts, "chewie")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusConflict {
				t.Errorf("phase %s: got %d, want 409", phase, resp.StatusCode)
			}
		})
	}
}

// TestCompactSession_404Unknown: no such agent.
func TestCompactSession_404Unknown(t *testing.T) {
	api.ResetCompactSessionCooldown()
	srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd})
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	resp := postCompact(t, ts, "ghost")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestCompactSession_503NoClientset: the exec stack isn't wired.
func TestCompactSession_503NoClientset(t *testing.T) {
	api.ResetCompactSessionCooldown()
	agent := runningCompactAgent("chewie", "claude-code")

	srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	resp := postCompact(t, ts, "chewie")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
}

// TestCompactSession_429Cooldown: a second call inside the window is
// throttled and says when to retry. Compaction costs a full-conversation
// model round-trip, so an unthrottled double-click is expensive in a way a
// double restart is not.
func TestCompactSession_429Cooldown(t *testing.T) {
	api.ResetCompactSessionCooldown()
	agent := runningCompactAgent("chewie", "claude-code")

	srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd}, agent)
	api.StampCompactSessionForTest("chewie")

	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	resp := postCompact(t, ts, "chewie")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status: got %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
}

// TestCompactSession_CooldownIsPerAgent: throttling one agent must not
// throttle another. The cooldown map is keyed by name; a shared timestamp
// would make one operator's compaction block everyone else's.
func TestCompactSession_CooldownIsPerAgent(t *testing.T) {
	api.ResetCompactSessionCooldown()
	chewie := runningCompactAgent("chewie", "claude-code")
	han := runningCompactAgent("han", "claude-code")

	srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd}, chewie, han)
	api.StampCompactSessionForTest("chewie")

	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	// chewie is in cooldown...
	resp := postCompact(t, ts, "chewie")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("chewie: got %d, want 429", resp.StatusCode)
	}
	// ...han is not, and reaches the 503 branch.
	resp2 := postCompact(t, ts, "han")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("han: got %d, want 503 (cooldown leaked across agents)", resp2.StatusCode)
	}
}

// TestCompactSession_CooldownRaceSerializes: the check and the stamp are
// atomic, so N concurrent requests produce exactly one claim winner. Same
// regression guard as the restart-session race test — a check-then-stamp
// pair would let several requests through the gap and deliver several
// /compact keystrokes into one TUI.
func TestCompactSession_CooldownRaceSerializes(t *testing.T) {
	api.ResetCompactSessionCooldown()
	agent := runningCompactAgent("chewie", "claude-code")

	srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd}, agent)
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
				ts.URL+"/api/v1/agents/chewie/compact-session", nil)
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("POST: %v", err)
				return
			}
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusServiceUnavailable:
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

// TestCompactSession_CooldownIndependentOfRestart: compacting must not
// consume the restart-session budget or vice versa. They are separate maps
// with different windows; sharing one would mean a compaction blocks the
// recovery action an operator reaches for when compaction doesn't help.
func TestCompactSession_CooldownIndependentOfRestart(t *testing.T) {
	api.ResetCompactSessionCooldown()
	api.ResetRestartSessionCooldown()
	agent := runningCompactAgent("chewie", "claude-code")

	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(defaultMachine(), agent).Build()
	srv := &api.Server{
		K8sClient:              fakeClient,
		MessageBuffer:          messagebuffer.NewMemoryBuffer(),
		APIKey:                 testAPIKey,
		Namespace:              "kyber-system",
		ValidRuntimes:          map[string]bool{"claude-code": true},
		CompactSessionCommands: map[string][]string{"claude-code": compactCmd},
		RestartSessionCommands: map[string][]string{"claude-code": {"/bin/bash", "/persist/last-claude-launch.sh"}},
	}
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	// Burn the compact cooldown.
	api.StampCompactSessionForTest("chewie")
	resp := postCompact(t, ts, "chewie")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("compact: got %d, want 429", resp.StatusCode)
	}

	// restart-session must still be reachable (503 = past all its guards).
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/chewie/restart-session", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST restart-session: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("restart-session: got %d, want 503 (compact cooldown leaked into restart)", resp2.StatusCode)
	}
}

// TestCompactSession_405OnGet guards the method check — compaction is a
// mutation and must not be reachable by a GET (a prefetch or a link crawler
// would otherwise trigger it).
func TestCompactSession_405OnGet(t *testing.T) {
	api.ResetCompactSessionCooldown()
	agent := runningCompactAgent("chewie", "claude-code")

	srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/agents/chewie/compact-session", nil)
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
