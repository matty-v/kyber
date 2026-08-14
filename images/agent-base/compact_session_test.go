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

// compactScriptPath is the absolute path to kyber-compact-session. It
// sources kyber-tmux-paste.sh from its own directory, so running it
// straight out of the repo exercises the same resolution the image uses.
func compactScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(dispatchRepoRoot(t), "images/agent-base/scripts/kyber-compact-session")
}

// compactHarness scaffolds a lock dir and a stubbed tmux, then runs the
// compact script against them. Deliberately leaner than dispatchHarness:
// this script has no prompt files, no event POST, and no cron markers.
type compactHarness struct {
	t       *testing.T
	root    string
	lockDir string
	tmuxLog string
	pathEnv string
}

func setupCompactHarness(t *testing.T) *compactHarness {
	t.Helper()
	root := t.TempDir()
	lockDir := filepath.Join(root, "lock")
	stubBin := filepath.Join(root, "stubbin")
	for _, d := range []string{lockDir, stubBin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tmuxLog := filepath.Join(root, "tmux.log")

	// Same stub contract as the dispatcher's, so both scripts are tested
	// against one behavioral model of tmux.
	writeStub(t, filepath.Join(stubBin, "tmux"), `#!/bin/bash
echo "tmux $*" >> "`+tmuxLog+`"
case "$1" in
    has-session)
        [ "${TMUX_STUB_MODE:-ok}" = "absent" ] && exit 1
        exit 0
        ;;
    load-buffer)
        cat >/dev/null
        [ "${TMUX_STUB_MODE:-ok}" = "load-fail" ] && exit 1
        exit 0
        ;;
    paste-buffer)
        [ "${TMUX_STUB_MODE:-ok}" = "paste-fail" ] && exit 1
        exit 0
        ;;
    send-keys)
        [ "${TMUX_STUB_MODE:-ok}" = "enter-fail" ] && exit 1
        exit 0
        ;;
esac
exit 0
`)
	writeStub(t, filepath.Join(stubBin, "sleep"), `#!/bin/bash
exit 0
`)

	return &compactHarness{
		t:       t,
		root:    root,
		lockDir: lockDir,
		tmuxLog: tmuxLog,
		pathEnv: stubBin + ":" + os.Getenv("PATH"),
	}
}

// run invokes the compact script, returning combined output and exit code.
func (h *compactHarness) run(args []string, extraEnv ...string) (string, int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{compactScriptPath(h.t)}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+h.pathEnv,
		"KYBER_JOBS_LOCK_DIR="+h.lockDir,
		"KYBER_JOBS_TMUX_SESSION=agent",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatalf("run: %v", err)
	}
	return string(out), code
}

// tmuxCalls returns the recorded tmux invocations.
func (h *compactHarness) tmuxCalls() string {
	b, err := os.ReadFile(h.tmuxLog)
	if err != nil {
		return ""
	}
	return string(b)
}

// TestCompact_DefaultsToSlashCompact: invoked with no argument, the script
// sends "/compact". Both shipped runtimes pass it explicitly, but the
// default is what makes the script usable by hand during an incident.
func TestCompact_DefaultsToSlashCompact(t *testing.T) {
	h := setupCompactHarness(t)
	out, code := h.run(nil)
	if code != 0 {
		t.Fatalf("exit: got %d, want 0 (out: %s)", code, out)
	}
	calls := h.tmuxCalls()
	if !strings.Contains(calls, "load-buffer") {
		t.Errorf("expected a load-buffer call, got: %s", calls)
	}
	if !strings.Contains(out, "/compact") {
		t.Errorf("expected the sent command in stdout, got: %s", out)
	}
}

// TestCompact_UsesBracketedPaste is the load-bearing assertion of this
// file. Codex's TUI absorbs a bare send-keys burst's trailing Enter, which
// strands the command in its editor and makes compaction silently do
// nothing. The delivery MUST be paste-buffer with -p (explicit paste-end
// marker) and a SEPARATE Enter.
func TestCompact_UsesBracketedPaste(t *testing.T) {
	h := setupCompactHarness(t)
	if _, code := h.run(nil); code != 0 {
		t.Fatalf("exit: got %d, want 0", code)
	}
	calls := h.tmuxCalls()
	if !strings.Contains(calls, "paste-buffer -p") {
		t.Errorf("expected bracketed paste (paste-buffer -p), got: %s", calls)
	}
	if !strings.Contains(calls, "send-keys -t agent Enter") {
		t.Errorf("expected a separate Enter keystroke, got: %s", calls)
	}
}

// TestCompact_CustomCommandText: the command is an argument so a runtime
// whose slash command diverges needs no new script.
func TestCompact_CustomCommandText(t *testing.T) {
	h := setupCompactHarness(t)
	out, code := h.run([]string{"/summarize"})
	if code != 0 {
		t.Fatalf("exit: got %d, want 0 (out: %s)", code, out)
	}
	if !strings.Contains(out, "/summarize") {
		t.Errorf("expected the custom command in stdout, got: %s", out)
	}
}

// TestCompact_SessionAbsentIsExit4: no tmux session means no live context.
// This must be a non-zero exit — the control-plane turns it into a visible
// error rather than a 200 that implies compaction happened.
func TestCompact_SessionAbsentIsExit4(t *testing.T) {
	h := setupCompactHarness(t)
	out, code := h.run(nil, "TMUX_STUB_MODE=absent")
	if code != 4 {
		t.Errorf("exit: got %d, want 4 (out: %s)", code, out)
	}
	if strings.Contains(h.tmuxCalls(), "load-buffer") {
		t.Error("must not attempt a paste when the session is absent")
	}
}

// TestCompact_SessionLockHeldIsExit3: a compact must not race the restart
// handler's kill+relaunch — the session it would paste into is about to be
// destroyed. Same guard the cron dispatcher honors (#128 / #135 D9).
func TestCompact_SessionLockHeldIsExit3(t *testing.T) {
	h := setupCompactHarness(t)
	lockPath := filepath.Join(h.lockDir, "session.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Hold the flock from a background process for the duration of the run.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	holder := exec.CommandContext(ctx, "flock", lockPath, "sleep", "5")
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	// Give flock a moment to actually acquire before racing it.
	waitForLockHeld(t, lockPath)

	out, code := h.run(nil)
	if code != 3 {
		t.Errorf("exit: got %d, want 3 (out: %s)", code, out)
	}
	if strings.Contains(h.tmuxCalls(), "load-buffer") {
		t.Error("must not paste while a session restart is in progress")
	}
}

// waitForLockHeld blocks until flock reports the lock as taken, so the test
// isn't racing the helper process's startup.
func waitForLockHeld(t *testing.T, lockPath string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// `flock -n <file> true` exits non-zero when the lock is held.
		if err := exec.Command("flock", "-n", lockPath, "true").Run(); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("lock holder never acquired the lock")
}

// TestCompact_DeliveryFailuresAreExit5 covers each tmux failure mode. The
// distinct exit code matters: 5 means "the session is there but tmux
// refused", which is a different operator response than 3 or 4.
func TestCompact_DeliveryFailuresAreExit5(t *testing.T) {
	for _, mode := range []string{"load-fail", "paste-fail", "enter-fail"} {
		t.Run(mode, func(t *testing.T) {
			h := setupCompactHarness(t)
			out, code := h.run(nil, "TMUX_STUB_MODE="+mode)
			if code != 5 {
				t.Errorf("exit: got %d, want 5 (out: %s)", code, out)
			}
			if !strings.Contains(out, "delivery failed") {
				t.Errorf("expected a delivery-failed message, got: %s", out)
			}
		})
	}
}

// TestCompact_TooManyArgsIsExit2 guards the usage check.
func TestCompact_TooManyArgsIsExit2(t *testing.T) {
	h := setupCompactHarness(t)
	_, code := h.run([]string{"/compact", "extra"})
	if code != 2 {
		t.Errorf("exit: got %d, want 2", code)
	}
}

// TestCompact_EmptyCommandIsExit2: an empty argument would paste nothing
// and then press Enter, submitting whatever the operator had half-typed in
// the agent's input box. Refuse instead.
func TestCompact_EmptyCommandIsExit2(t *testing.T) {
	h := setupCompactHarness(t)
	_, code := h.run([]string{""})
	if code != 2 {
		t.Errorf("exit: got %d, want 2", code)
	}
	if strings.Contains(h.tmuxCalls(), "load-buffer") {
		t.Error("must not paste an empty command")
	}
}

// TestCompact_NoSessionLockFileIsFine: a fresh pod has never restarted its
// session, so session.lock doesn't exist yet. That must not be read as
// "locked" — the guard is `[ -f ] && ! flock`, and an inverted check here
// would make compaction impossible until the first restart.
func TestCompact_NoSessionLockFileIsFine(t *testing.T) {
	h := setupCompactHarness(t)
	if _, err := os.Stat(filepath.Join(h.lockDir, "session.lock")); !os.IsNotExist(err) {
		t.Fatal("precondition: session.lock should not exist")
	}
	out, code := h.run(nil)
	if code != 0 {
		t.Errorf("exit: got %d, want 0 (out: %s)", code, out)
	}
}
