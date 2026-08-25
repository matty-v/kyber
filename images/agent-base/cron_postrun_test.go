package agent_base_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// kyber-cron-postrun is the platform's Stop hook for scheduled jobs. These
// tests drive the real script with a stubbed clear command, asserting on the
// two things it owns: firing the clear when a job asked for one, and always
// removing the pending marker (which is what releases the --exclusive
// agent-busy guard in kyber-job-dispatch).

type postrunHarness struct {
	t        *testing.T
	pendDir  string
	logFile  string
	clearCmd string // stub script path
	clearLog string // records each stub invocation
}

func postrunScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(dispatchRepoRoot(t), "images/agent-base/scripts/kyber-cron-postrun")
}

func setupPostrunHarness(t *testing.T) *postrunHarness {
	t.Helper()
	root := t.TempDir()
	h := &postrunHarness{
		t:        t,
		pendDir:  filepath.Join(root, "pending"),
		logFile:  filepath.Join(root, "postrun.log"),
		clearCmd: filepath.Join(root, "clear-stub"),
		clearLog: filepath.Join(root, "clear.log"),
	}
	if err := os.MkdirAll(h.pendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, h.clearCmd, `#!/bin/bash
echo "clear $*" >> "`+h.clearLog+`"
exit ${CLEAR_STUB_RC:-0}
`)
	return h
}

func (h *postrunHarness) marker(job, body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.pendDir, job), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *postrunHarness) run(extraEnv ...string) (string, int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", postrunScriptPath(h.t))
	cmd.Env = append(os.Environ(),
		"KYBER_CRON_PENDING_DIR="+h.pendDir,
		"KYBER_CRON_POSTRUN_LOG="+h.logFile,
		"KYBER_CLEAR_SESSION_CMD="+h.clearCmd,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			h.t.Fatalf("run: %v (output: %s)", err, out)
		}
	}
	return string(out), code
}

func (h *postrunHarness) clearInvocations() string {
	h.t.Helper()
	b, _ := os.ReadFile(h.clearLog)
	return string(b)
}

func (h *postrunHarness) logContents() string {
	h.t.Helper()
	b, _ := os.ReadFile(h.logFile)
	return string(b)
}

func TestCronPostrun_ClearsWhenRequested(t *testing.T) {
	h := setupPostrunHarness(t)
	h.marker("work-tick", "started_at=2026-08-24T10:00:00Z\nclear_context=true\n")

	if _, code := h.run(); code != 0 {
		t.Fatalf("hook must always exit 0, got %d", code)
	}

	if inv := h.clearInvocations(); !strings.Contains(inv, "/clear") {
		t.Errorf("want the runtime clear command invoked, got %q", inv)
	}
	if _, err := os.Stat(filepath.Join(h.pendDir, "work-tick")); !os.IsNotExist(err) {
		t.Errorf("marker must be removed after the hook runs (err=%v)", err)
	}
}

// A job that did not ask for a clear must still have its marker removed —
// that removal is what un-blocks the next --exclusive fire.
func TestCronPostrun_RemovesMarkerWithoutClearing(t *testing.T) {
	h := setupPostrunHarness(t)
	h.marker("digest", "started_at=2026-08-24T10:00:00Z\nclear_context=false\n")

	if _, code := h.run(); code != 0 {
		t.Fatalf("hook must always exit 0, got %d", code)
	}

	if inv := h.clearInvocations(); inv != "" {
		t.Errorf("no clear was requested, but the command ran: %q", inv)
	}
	if _, err := os.Stat(filepath.Join(h.pendDir, "digest")); !os.IsNotExist(err) {
		t.Errorf("marker must be removed even with no clear (err=%v)", err)
	}
}

// The context is shared, so two jobs finishing in the same turn must not
// produce two clears — the second would land in an already-empty session and
// read as a stray prompt.
func TestCronPostrun_ClearsAtMostOncePerTurn(t *testing.T) {
	h := setupPostrunHarness(t)
	h.marker("job-a", "clear_context=true\n")
	h.marker("job-b", "clear_context=true\n")

	if _, code := h.run(); code != 0 {
		t.Fatalf("hook must always exit 0, got %d", code)
	}

	if got := strings.Count(h.clearInvocations(), "clear "); got != 1 {
		t.Errorf("want exactly 1 clear for 2 markers, got %d (%q)", got, h.clearInvocations())
	}
	for _, j := range []string{"job-a", "job-b"} {
		if _, err := os.Stat(filepath.Join(h.pendDir, j)); !os.IsNotExist(err) {
			t.Errorf("marker %s must be removed (err=%v)", j, err)
		}
	}
}

// A failing clear must not strand the marker: leaving it would mute the job's
// schedule until the staleness TTL, which is worse than a missed clear.
func TestCronPostrun_RemovesMarkerWhenClearFails(t *testing.T) {
	h := setupPostrunHarness(t)
	h.marker("work-tick", "clear_context=true\n")

	if _, code := h.run("CLEAR_STUB_RC=4"); code != 0 {
		t.Fatalf("hook must always exit 0 even when the clear fails, got %d", code)
	}

	if _, err := os.Stat(filepath.Join(h.pendDir, "work-tick")); !os.IsNotExist(err) {
		t.Errorf("marker must be removed even when the clear fails (err=%v)", err)
	}
	if log := h.logContents(); !strings.Contains(log, "event=clear_failed") || !strings.Contains(log, "rc=4") {
		t.Errorf("want the failure recorded with its exit code, got: %s", log)
	}
}

func TestCronPostrun_NoMarkersIsANoOp(t *testing.T) {
	h := setupPostrunHarness(t)

	if _, code := h.run(); code != 0 {
		t.Fatalf("hook must always exit 0, got %d", code)
	}
	if inv := h.clearInvocations(); inv != "" {
		t.Errorf("no markers means no clear, got %q", inv)
	}
}

// A runtime with no clear command must be reported, not guessed at.
func TestCronPostrun_SkipsWhenRuntimeHasNoClearCommand(t *testing.T) {
	h := setupPostrunHarness(t)
	h.marker("work-tick", "clear_context=true\n")

	if _, code := h.run("KYBER_CLEAR_SESSION_TEXT="); code != 0 {
		t.Fatalf("hook must always exit 0, got %d", code)
	}

	if inv := h.clearInvocations(); inv != "" {
		t.Errorf("must not invoke a clear with no command text, got %q", inv)
	}
	if log := h.logContents(); !strings.Contains(log, "runtime_has_no_clear_command") {
		t.Errorf("want the skip reason logged, got: %s", log)
	}
}
