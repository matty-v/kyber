//go:build integration

package agent_base_test

import (
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

// runHermesGoalHook feeds a payload to the hook and returns how many
// /goal-start calls the server saw in total.
func runHermesGoalHook(t *testing.T, url, persistRoot, payload string) error {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", filepath.Join(wd, "scripts", "kyber-hermes-goal-start"))
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"KYBER_GOAL_START_URL="+url,
		"KYBER_PERSIST_ROOT="+persistRoot,
	)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		t.Logf("hook output: %s", out)
	}
	return err
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
		if err := runHermesGoalHook(t, srv.URL, persist, payload("turn_1")); err != nil {
			t.Fatalf("hook failed: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("goal-start called %d times within one turn, want 1", got)
	}

	if err := runHermesGoalHook(t, srv.URL, persist, payload("turn_2")); err != nil {
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

	if err := runHermesGoalHook(t, srv.URL, persist, `{"session_id":"sess_a","extra":{"turn_id":"turn_1"}}`); err != nil {
		t.Fatal(err)
	}
	if err := runHermesGoalHook(t, srv.URL, persist, `{"session_id":"sess_b","extra":{"turn_id":"turn_1"}}`); err != nil {
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

	if err := runHermesGoalHook(t, srv.URL, t.TempDir(), `{"session_id":"sess_a"}`); err != nil {
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

	if err := runHermesGoalHook(t, srv.URL, persist, payload); err != nil {
		t.Fatalf("a failed open must not fail the hook: %v", err)
	}
	fail.Store(false)
	if err := runHermesGoalHook(t, srv.URL, persist, payload); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("goal-start called %d times, want 2 — a failed open must be retried", got)
	}
	// And now that it succeeded, the turn is recorded.
	if err := runHermesGoalHook(t, srv.URL, persist, payload); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("goal-start called %d times after success, want 2", got)
	}
}

// The hook must never block or fail a turn, whatever the endpoint does.
func TestHermesGoalHookNeverFailsTheTurn(t *testing.T) {
	if err := runHermesGoalHook(t, "http://127.0.0.1:1/goal-start", t.TempDir(),
		`{"session_id":"s","extra":{"turn_id":"t"}}`); err != nil {
		t.Fatalf("unreachable endpoint failed the hook: %v", err)
	}
}
