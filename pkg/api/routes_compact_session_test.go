package api_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestCompactSession_CooldownClaimIsAtomic: the check and the stamp happen
// under one lock, so N concurrent claims produce exactly one winner. A
// check-then-stamp pair would let several requests through the gap and
// deliver several /compact keystrokes into one TUI.
//
// Driven against the claim directly rather than through HTTP: a failed
// delivery now RELEASES the claim, so concurrent requests that all fail all
// end up claiming in turn, and the HTTP status can no longer stand in for
// "who won the claim".
func TestCompactSession_CooldownClaimIsAtomic(t *testing.T) {
	api.ResetCompactSessionCooldown()

	const parallel = 32
	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	now := time.Now()
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximize overlap
			if _, claimed := api.TryClaimCompactForTest("chewie", now); claimed {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Errorf("claim winners: got %d, want 1 (cooldown not atomic — race present)", got)
	}
}

// TestCompactSession_FailedDeliveryReleasesCooldown is the regression guard
// for the nastiest version of the bug: the 501 tells the operator to roll
// the agent onto a newer image, and their retry — after doing exactly that —
// used to be refused for 60s over a compaction that never happened. Nothing
// was delivered, so nothing should be throttled.
//
// Uses the 503 branch (no clientset) as the stand-in for any post-claim
// failure; it is past the claim and delivers nothing, same as the rest.
func TestCompactSession_FailedDeliveryReleasesCooldown(t *testing.T) {
	api.ResetCompactSessionCooldown()
	agent := runningCompactAgent("chewie", "claude-code")

	srv := newCompactSessionServer(t, map[string][]string{"claude-code": compactCmd}, agent)
	ts := httptest.NewServer(srv.BuildHandler())
	defer ts.Close()

	resp := postCompact(t, ts, "chewie")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first call: got %d, want 503", resp.StatusCode)
	}
	if api.HasCompactClaimForTest("chewie") {
		t.Error("a failed delivery must not hold the cooldown")
	}

	// The retry must reach the same branch, not a 429.
	resp2 := postCompact(t, ts, "chewie")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("retry: got %d, want 503 (cooldown held after a failed delivery)", resp2.StatusCode)
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

// TestCompactSession_MissingScriptDetection covers the stderr classifier
// that turns "exit code 1" into an actionable answer. The real message is
// the one runuser produced on a live codex pod:
//
//	runuser: failed to execute /usr/local/bin/kyber-compact-session: No such file or directory
//
// Getting this wrong in either direction is bad: a false negative sends the
// operator back to "compaction is broken", and a false positive tells them
// to upgrade an image that is already fine.
func TestCompactSession_MissingScriptDetection(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "runuser cannot execute the script",
			stderr: "runuser: failed to execute /usr/local/bin/kyber-compact-session: No such file or directory\n",
			want:   true,
		},
		{
			name:   "shell cannot execute the script itself",
			stderr: "bash: /usr/local/bin/kyber-compact-session: command not found\n",
			want:   true,
		},
		{
			// The script RAN and something it calls is missing. Both the
			// script's name and "command not found" appear, but the line
			// number proves it executed — telling this operator to roll onto
			// a newer image sends them to fix the one thing that isn't broken.
			name:   "script ran but a command it needs is missing",
			stderr: "/usr/local/bin/kyber-compact-session: line 60: flock: command not found\n",
			want:   false,
		},
		{
			name:   "empty stderr is not a missing script",
			stderr: "",
			want:   false,
		},
		{
			// The script ran and refused — a real, different condition with
			// its own remedy (wait for the restart to finish).
			name:   "script ran and reported a restart in progress",
			stderr: "kyber-compact-session: session restart in progress; not compacting\n",
			want:   false,
		},
		{
			name:   "script ran and reported an absent tmux session",
			stderr: "kyber-compact-session: tmux session 'agent' is absent\n",
			want:   false,
		},
		{
			// Some other binary is missing. Still broken, but telling the
			// operator to roll the agent image would be a guess.
			name:   "a different missing file",
			stderr: "nsenter: failed to execute /usr/sbin/runuser: No such file or directory\n",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := api.IsMissingInPodScriptForTest(tc.stderr); got != tc.want {
				t.Errorf("got %v, want %v for stderr %q", got, tc.want, tc.stderr)
			}
		})
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
