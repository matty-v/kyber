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

// dispatcherScriptPath is the absolute path to the kyber-job-dispatch script
// under test. Tests run it directly via bash with overridden env-var paths.
func dispatcherScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(dispatchRepoRoot(t), "images/agent-base/scripts/kyber-job-dispatch")
}

// dispatchRepoRoot resolves the repo root via `git rev-parse`. Duplicated
// here rather than calling the repoRootDir helper in home_persistence_test.go
// because that file is gated behind the `docker_integration` build tag and
// isn't compiled into the default test run.
func dispatchRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v (%s)", err, out)
	}
	return strings.TrimSpace(string(out))
}

// dispatchHarness builds a temp dir scaffold (persist + jobs-src) and a
// PATH-prepended stub directory holding fake tmux/curl/jq/date/flock, so the
// test can assert on side-effects without touching the real system.
type dispatchHarness struct {
	t        *testing.T
	root     string // temp root
	persist  string // $root/persist
	jobsSrc  string // $root/jobs-src
	stubBin  string // $root/stubbin (prepended to PATH)
	logFile  string
	lockDir  string
	tmuxLog  string
	curlLog  string
	cronDir  string // $persist/var/run/kyber-cron-complete (completion markers)
	pathEnv  string
	baseArgs []string // script args (after flags)
}

// setupDispatchHarness creates the scaffolding. Stubs for tmux/curl/jq are
// real binaries on the host if available; where we need to intercept (tmux,
// curl) we write shell wrappers that log their invocations.
func setupDispatchHarness(t *testing.T) *dispatchHarness {
	t.Helper()
	root := t.TempDir()
	persist := filepath.Join(root, "persist")
	jobsSrc := filepath.Join(root, "jobs-src")
	stubBin := filepath.Join(root, "stubbin")
	for _, d := range []string{persist, filepath.Join(persist, "var", "log"), filepath.Join(persist, "var", "lock"), jobsSrc, stubBin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tmuxLog := filepath.Join(root, "tmux.log")
	curlLog := filepath.Join(root, "curl.log")

	// tmux stub: records each invocation to $tmuxLog. has-session returns 0
	// so the dispatcher proceeds to paste. Callers override the has-
	// session behavior for the session-absent test by overriding TMUX_STUB_MODE.
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
esac
exit 0
`)
	writeStub(t, filepath.Join(stubBin, "sleep"), `#!/bin/bash
echo "sleep $*" >> "`+tmuxLog+`"
exit 0
`)

	// curl stub: records the URL it was invoked against.
	writeStub(t, filepath.Join(stubBin, "curl"), `#!/bin/bash
echo "curl $*" >> "`+curlLog+`"
# Consume stdin (the request body) so the caller's pipe doesn't SIGPIPE.
cat >/dev/null
exit 0
`)

	// Resolve required real binaries up front so PATH prep can point at them.
	pathEnv := stubBin + ":" + os.Getenv("PATH")

	return &dispatchHarness{
		t:       t,
		root:    root,
		persist: persist,
		jobsSrc: jobsSrc,
		stubBin: stubBin,
		logFile: filepath.Join(persist, "var", "log", "kyber-jobs.log"),
		lockDir: filepath.Join(persist, "var", "lock"),
		tmuxLog: tmuxLog,
		curlLog: curlLog,
		cronDir: filepath.Join(persist, "var", "run", "kyber-cron-complete"),
		pathEnv: pathEnv,
	}
}

func writeStub(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", path, err)
	}
}

// run invokes the dispatcher script with the given args + extra env vars.
// Returns combined stdout/stderr + exit code. Always uses a 10s timeout so
// a hang in the script doesn't wedge the test binary.
func (h *dispatchHarness) run(args []string, extraEnv ...string) (string, int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{dispatcherScriptPath(h.t)}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+h.pathEnv,
		"AGENT_NAME=chewie",
		"KYBER_CONTROL_PLANE_INTERNAL_URL=http://cp.test",
		"KYBER_POD_TOKEN_FILE=/dev/null",
		"KYBER_JOBS_LOG_FILE="+h.logFile,
		"KYBER_JOBS_LOCK_DIR="+h.lockDir,
		"KYBER_JOBS_SRC_DIR="+h.jobsSrc,
		"KYBER_JOBS_TMUX_SESSION=agent",
		"KYBER_CRON_COMPLETE_DIR="+h.cronDir,
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

// runStdin invokes the dispatcher with the given args and pipes `stdin` in
// as the prompt body. Mirrors `run` but writes to the child's stdin pipe
// before reading combined output. Used to exercise the --stdin code path.
func (h *dispatchHarness) runStdin(args []string, stdin string, extraEnv ...string) (string, int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{dispatcherScriptPath(h.t)}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+h.pathEnv,
		"AGENT_NAME=chewie",
		"KYBER_CONTROL_PLANE_INTERNAL_URL=http://cp.test",
		"KYBER_POD_TOKEN_FILE=/dev/null",
		"KYBER_JOBS_LOG_FILE="+h.logFile,
		"KYBER_JOBS_LOCK_DIR="+h.lockDir,
		"KYBER_JOBS_SRC_DIR="+h.jobsSrc,
		"KYBER_JOBS_TMUX_SESSION=agent",
		"KYBER_CRON_COMPLETE_DIR="+h.cronDir,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			h.t.Fatalf("runStdin: %v (output: %s)", err, out)
		}
	}
	return string(out), code
}

func (h *dispatchHarness) writePrompt(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.jobsSrc, "prompt-"+name), []byte(content), 0o644); err != nil {
		h.t.Fatalf("write prompt: %v", err)
	}
}

func (h *dispatchHarness) logContents() string {
	h.t.Helper()
	b, err := os.ReadFile(h.logFile)
	if err != nil {
		return ""
	}
	return string(b)
}

func (h *dispatchHarness) curlContents() string {
	h.t.Helper()
	b, _ := os.ReadFile(h.curlLog)
	return string(b)
}

// TestKyberJobDispatch_Success confirms the happy path: prompt file present,
// tmux session up, prompt bracket-pasted + Enter, log line + POST emitted.
func TestKyberJobDispatch_Success(t *testing.T) {
	h := setupDispatchHarness(t)
	h.writePrompt("morning", "hello world\n`dangerous`")

	out, code := h.run([]string{"morning"})
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (output: %s)", code, out)
	}

	log := h.logContents()
	if !strings.Contains(log, "job=morning event=success") {
		t.Errorf("expected success log line, got:\n%s", log)
	}

	tmuxOut, _ := os.ReadFile(h.tmuxLog)
	if !strings.Contains(string(tmuxOut), "load-buffer -b kyber-morning -") {
		t.Errorf("expected tmux buffer load, got:\n%s", tmuxOut)
	}
	if !strings.Contains(string(tmuxOut), "paste-buffer -p -d -b kyber-morning -t agent") {
		t.Errorf("expected bracketed paste, got:\n%s", tmuxOut)
	}
	if !strings.Contains(string(tmuxOut), "sleep 0.1") {
		t.Errorf("expected paste-settle delay before Enter, got:\n%s", tmuxOut)
	}
	if !strings.Contains(string(tmuxOut), "send-keys -t agent Enter") {
		t.Errorf("expected Enter send-keys call, got:\n%s", tmuxOut)
	}

	if !strings.Contains(h.curlContents(), "http://cp.test/internal/agents/chewie/job-events") {
		t.Errorf("expected POST to job-events endpoint, got:\n%s", h.curlContents())
	}
}

// TestKyberJobDispatch_PromptMissing records outcome=failed when the ConfigMap
// doesn't carry a prompt for the named job (e.g., spec.jobs was edited but
// the ConfigMap hasn't propagated yet).
func TestKyberJobDispatch_PromptMissing(t *testing.T) {
	h := setupDispatchHarness(t)
	// No prompt-morning file.

	out, code := h.run([]string{"morning"})
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (script must not fail the crontab line) — %s", code, out)
	}
	if !strings.Contains(h.logContents(), "event=failed error=prompt_file_missing") {
		t.Errorf("expected failed/prompt_file_missing, got:\n%s", h.logContents())
	}
}

// TestKyberJobDispatch_TmuxAbsent records outcome=failed when the agent's
// tmux session isn't running.
func TestKyberJobDispatch_TmuxAbsent(t *testing.T) {
	h := setupDispatchHarness(t)
	h.writePrompt("morning", "hi")

	out, code := h.run([]string{"morning"}, "TMUX_STUB_MODE=absent")
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 — %s", code, out)
	}
	if !strings.Contains(h.logContents(), "event=failed error=tmux_session_absent") {
		t.Errorf("expected failed/tmux_session_absent, got:\n%s", h.logContents())
	}
}

func TestKyberJobDispatch_TmuxPasteFailures(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantErr string
	}{
		{"load buffer", "load-fail", "tmux_load_buffer_failed"},
		{"paste buffer", "paste-fail", "tmux_paste_buffer_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := setupDispatchHarness(t)
			h.writePrompt("morning", "hello")
			out, code := h.run([]string{"morning"}, "TMUX_STUB_MODE="+tc.mode)
			if code != 0 {
				t.Fatalf("exit code: got %d, want 0 (%s)", code, out)
			}
			if !strings.Contains(h.logContents(), "event=failed error="+tc.wantErr) {
				t.Errorf("expected %s, got:\n%s", tc.wantErr, h.logContents())
			}
		})
	}
}

// TestKyberJobDispatch_InvalidName rejects names that don't match the CRD
// regex — defense-in-depth for the run-now path.
func TestKyberJobDispatch_InvalidName(t *testing.T) {
	h := setupDispatchHarness(t)

	_, code := h.run([]string{"has spaces"})
	if code != 2 {
		t.Fatalf("exit code: got %d, want 2 for invalid name", code)
	}
}

// TestKyberJobDispatch_SessionLockHeld records outcome=skipped when the
// session-restart handler holds the session lock.
func TestKyberJobDispatch_SessionLockHeld(t *testing.T) {
	h := setupDispatchHarness(t)
	h.writePrompt("morning", "hi")

	sessionLock := filepath.Join(h.lockDir, "session.lock")
	f, err := os.Create(sessionLock)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Hold the flock from a bash subshell for the duration of this test.
	holderCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	holder := exec.CommandContext(holderCtx, "bash", "-c",
		"flock -n "+sessionLock+" sleep 5")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Process.Kill() //nolint:errcheck

	// Give flock a moment to acquire. A retry-until-held pattern would be
	// cleaner but this is deterministic enough for CI.
	time.Sleep(200 * time.Millisecond)

	out, code := h.run([]string{"morning"})
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (%s)", code, out)
	}
	if !strings.Contains(h.logContents(), "event=skipped error=session_restart_in_progress") {
		t.Errorf("expected skipped/session_restart_in_progress, got:\n%s", h.logContents())
	}
}

// TestKyberJobDispatch_StdinSuccess exercises the --stdin code path: the
// prompt is read from stdin instead of a prompt file, then sent into tmux
// with the same bracketed-paste + Enter sequence. No prompt file is written.
func TestKyberJobDispatch_StdinSuccess(t *testing.T) {
	h := setupDispatchHarness(t)
	// Deliberately no writePrompt — stdin mode must not require the file.

	out, code := h.runStdin([]string{"--stdin", "inbound-abc123"}, "hello world\n")
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (output: %s)", code, out)
	}

	log := h.logContents()
	if !strings.Contains(log, "job=inbound-abc123 event=success") {
		t.Errorf("expected success log line, got:\n%s", log)
	}

	tmuxOut, _ := os.ReadFile(h.tmuxLog)
	if !strings.Contains(string(tmuxOut), "load-buffer -b kyber-inbound-abc123 -") {
		t.Errorf("expected tmux buffer load, got:\n%s", tmuxOut)
	}
	if !strings.Contains(string(tmuxOut), "paste-buffer -p -d -b kyber-inbound-abc123 -t agent") {
		t.Errorf("expected bracketed paste, got:\n%s", tmuxOut)
	}
	if !strings.Contains(string(tmuxOut), "send-keys -t agent Enter") {
		t.Errorf("expected Enter send-keys call, got:\n%s", tmuxOut)
	}

	if !strings.Contains(h.curlContents(), "http://cp.test/internal/agents/chewie/job-events") {
		t.Errorf("expected POST to job-events endpoint, got:\n%s", h.curlContents())
	}
}

// TestKyberJobDispatch_StdinEmpty: zero-byte stdin in --stdin mode must
// record outcome=skipped reason=empty_prompt and skip tmux entirely (no
// stray Enter into the agent's session).
func TestKyberJobDispatch_StdinEmpty(t *testing.T) {
	h := setupDispatchHarness(t)

	out, code := h.runStdin([]string{"--stdin", "inbound-empty"}, "")
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (output: %s)", code, out)
	}
	if !strings.Contains(h.logContents(), "event=skipped error=empty_prompt") {
		t.Errorf("expected skipped/empty_prompt, got:\n%s", h.logContents())
	}

	// tmux must not have been touched at all in the empty case — no
	// has-session probe, no send-keys.
	tmuxOut, _ := os.ReadFile(h.tmuxLog)
	if len(tmuxOut) != 0 {
		t.Errorf("expected no tmux invocations, got:\n%s", tmuxOut)
	}
}

// TestKyberJobDispatch_StdinSkipsFileCheck confirms stdin mode bypasses
// the prompt-file presence check — there is intentionally no prompt file
// for the inbound job name, and the dispatch still succeeds.
func TestKyberJobDispatch_StdinSkipsFileCheck(t *testing.T) {
	h := setupDispatchHarness(t)

	out, code := h.runStdin([]string{"--stdin", "inbound-xyz"}, "do the thing\n")
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (output: %s)", code, out)
	}
	log := h.logContents()
	if strings.Contains(log, "prompt_file_missing") {
		t.Errorf("stdin mode must not check the prompt file, got:\n%s", log)
	}
	if !strings.Contains(log, "event=success") {
		t.Errorf("expected success, got:\n%s", log)
	}
}

// TestKyberJobDispatch_NamedJobWritesCompletionMarker verifies that a named
// scheduled job drops a completion marker (kyber#627) so the Stop hook can
// notify the operator when the agent finishes.
func TestKyberJobDispatch_NamedJobWritesCompletionMarker(t *testing.T) {
	h := setupDispatchHarness(t)
	h.writePrompt("compact-memory", "run /compact-memory\n")

	out, code := h.run([]string{"compact-memory"})
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (output: %s)", code, out)
	}

	marker := filepath.Join(h.cronDir, "compact-memory")
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected completion marker at %s: %v", marker, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		t.Errorf("marker should contain the started-at timestamp, got empty")
	}
}

// TestKyberJobDispatch_InboundWritesNoMarker verifies that inbound (--stdin)
// dispatches do NOT drop a completion marker — they are event-driven, not
// operator crons, and would otherwise spam the operator (kyber#627).
func TestKyberJobDispatch_InboundWritesNoMarker(t *testing.T) {
	h := setupDispatchHarness(t)

	out, code := h.runStdin([]string{"--stdin", "inbound-req123"}, "some inbound envelope\n")
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0 (output: %s)", code, out)
	}

	if _, err := os.Stat(filepath.Join(h.cronDir, "inbound-req123")); !os.IsNotExist(err) {
		t.Errorf("inbound dispatch must not write a completion marker (err=%v)", err)
	}
}
