//go:build integration

package hermes_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// runHermesGoalHook feeds a payload to the hook and returns its stdout, which
// is the {"context": ...} Hermes injects into the model's context.
func runHermesGoalHook(t *testing.T, url, persistRoot, payload string) (string, error) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", filepath.Join(wd, "..", "agent-base", "scripts", "kyber-hermes-goal-start"))
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"KYBER_GOAL_START_URL="+url,
		"KYBER_PERSIST_ROOT="+persistRoot,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if stderr.Len() > 0 {
		t.Logf("hook stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

func goalStartServer(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"acceptedAt":"2026-09-22T05:00:00Z"}`))
	}))
}

// pre_llm_call fires on EVERY model call. Opening a revision each time would
// reset the goal to the platform placeholder mid-turn, repeatedly wiping the
// summary the agent just set — so only the first call of a turn may fire.
func TestHermesGoalHookOpensOncePerTurn(t *testing.T) {
	var calls int32
	srv := goalStartServer(t, &calls)
	defer srv.Close()
	persist := t.TempDir()

	payload := func(turn string) string {
		return fmt.Sprintf(`{"session_id":"sess_a","extra":{"turn_id":%q}}`, turn)
	}

	for i := 0; i < 3; i++ {
		if _, err := runHermesGoalHook(t, srv.URL, persist, payload("turn_1")); err != nil {
			t.Fatalf("hook failed: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("goal-start called %d times within one turn, want 1", got)
	}

	if _, err := runHermesGoalHook(t, srv.URL, persist, payload("turn_2")); err != nil {
		t.Fatalf("hook failed: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("goal-start called %d times across two turns, want 2", got)
	}
}

// A restarted session can begin its turn ids again at the same value, so the
// session must be part of the dedup key or the first turn of the new session
// silently never opens a revision.
func TestHermesGoalHookTreatsANewSessionAsANewTurn(t *testing.T) {
	var calls int32
	srv := goalStartServer(t, &calls)
	defer srv.Close()
	persist := t.TempDir()

	if _, err := runHermesGoalHook(t, srv.URL, persist, `{"session_id":"sess_a","extra":{"turn_id":"turn_1"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := runHermesGoalHook(t, srv.URL, persist, `{"session_id":"sess_b","extra":{"turn_id":"turn_1"}}`); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("goal-start called %d times, want 2 — session must be part of the key", got)
	}
}

// With no turn id there is no way to tell a new turn from the third model call
// of the current one. Firing blind would clobber the agent's own goal mid-turn.
func TestHermesGoalHookDoesNothingWithoutATurnID(t *testing.T) {
	var calls int32
	srv := goalStartServer(t, &calls)
	defer srv.Close()

	if _, err := runHermesGoalHook(t, srv.URL, t.TempDir(), `{"session_id":"sess_a"}`); err != nil {
		t.Fatalf("hook must not fail: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("goal-start called %d times without a turn id, want 0", got)
	}
}

// A failed open must be retried on the next model call of the same turn, not
// silently skipped for the whole turn.
func TestHermesGoalHookRetriesAfterAFailedOpen(t *testing.T) {
	var calls int32
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"acceptedAt":"2026-09-22T05:00:00Z"}`))
	}))
	defer srv.Close()
	persist := t.TempDir()
	payload := `{"session_id":"sess_a","extra":{"turn_id":"turn_1"}}`

	if _, err := runHermesGoalHook(t, srv.URL, persist, payload); err != nil {
		t.Fatalf("a failed open must not fail the hook: %v", err)
	}
	fail.Store(false)
	if _, err := runHermesGoalHook(t, srv.URL, persist, payload); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("goal-start called %d times, want 2 — a failed open must be retried", got)
	}
	// And now that it succeeded, the turn is recorded.
	if _, err := runHermesGoalHook(t, srv.URL, persist, payload); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("goal-start called %d times after success, want 2", got)
	}
}

// The hook must never block or fail a turn, whatever the endpoint does.
func TestHermesGoalHookNeverFailsTheTurn(t *testing.T) {
	if _, err := runHermesGoalHook(t, "http://127.0.0.1:1/goal-start", t.TempDir(),
		`{"session_id":"s","extra":{"turn_id":"t"}}`); err != nil {
		t.Fatalf("unreachable endpoint failed the hook: %v", err)
	}
}

// Hermes passes a pre_llm_call hook's {"context": "..."} stdout into the
// model's context, so the agent is TOLD the revision — the same nudge the
// Claude Code hook delivers through additionalContext. Without this the agent
// would have to discover the revision on its own and the placeholder would
// usually stand.
func TestHermesGoalHookInjectsTheRevisionAsContext(t *testing.T) {
	var calls int32
	srv := goalStartServer(t, &calls)
	defer srv.Close()
	persist := t.TempDir()
	payload := `{"session_id":"sess_a","extra":{"turn_id":"turn_1"}}`

	out, err := runHermesGoalHook(t, srv.URL, persist, payload)
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}
	var got struct {
		Context string `json:"context"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not the {\"context\": ...} Hermes injects: %q", out)
	}
	if !strings.Contains(got.Context, "2026-09-22T05:00:00Z") {
		t.Errorf("context does not carry the revision: %q", got.Context)
	}
	if !strings.Contains(got.Context, "set_goal") {
		t.Errorf("context does not name the tool to call: %q", got.Context)
	}

	// Later calls in the same turn must inject nothing, or the nudge is
	// repeated on every model call of the turn.
	out, err = runHermesGoalHook(t, srv.URL, persist, payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("nudge re-injected within the same turn: %q", out)
	}
}

// The hook must never copy the user's prompt into the goal surface.
func TestHermesGoalHookNeverEchoesThePrompt(t *testing.T) {
	var calls int32
	srv := goalStartServer(t, &calls)
	defer srv.Close()

	out, err := runHermesGoalHook(t, srv.URL, t.TempDir(),
		`{"session_id":"s","extra":{"turn_id":"t"},"messages":[{"role":"user","content":"my secret deploy token is hunter2"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "secret deploy") {
		t.Fatalf("prompt body leaked into the injected context: %q", out)
	}
}
