package agent_base_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	h.marker("work-tick", "started_at=2026-08-24T10:00:00Z\nclear_context=true\nstate=armed\n")

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
	h.marker("digest", "started_at=2026-08-24T10:00:00Z\nclear_context=false\nstate=armed\n")

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
	h.marker("job-a", "clear_context=true\nstate=armed\n")
	h.marker("job-b", "clear_context=true\nstate=armed\n")

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
	h.marker("work-tick", "clear_context=true\nstate=armed\n")

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
	h.marker("work-tick", "clear_context=true\nstate=armed\n")

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

// --- Turn correlation (the busy-at-fire ordering) ---

func turnStartScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(dispatchRepoRoot(t), "images/agent-base/scripts/kyber-cron-turn-start")
}

// runTurnStart feeds a UserPromptSubmit payload to the arming hook.
func (h *postrunHarness) runTurnStart(prompt string) (string, int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", turnStartScriptPath(h.t))
	cmd.Env = append(os.Environ(),
		"KYBER_CRON_PENDING_DIR="+h.pendDir,
		"KYBER_CRON_POSTRUN_LOG="+h.logFile,
	)
	payload, err := json.Marshal(map[string]string{
		"hook_event_name": "UserPromptSubmit",
		"prompt":          prompt,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	cmd.Stdin = bytes.NewReader(payload)
	out, e := cmd.CombinedOutput()
	code := 0
	if e != nil {
		if ee, ok := e.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			h.t.Fatalf("runTurnStart: %v (output: %s)", e, out)
		}
	}
	return string(out), code
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (h *postrunHarness) markerBody(job string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.pendDir, job))
	if err != nil {
		h.t.Fatalf("read marker %s: %v", job, err)
	}
	return string(b)
}

// THE REGRESSION Luna caught. Cron fires while the agent is already mid-turn,
// so the scheduled prompt sits queued behind that work. When the unrelated turn
// ends, its Stop event must not consume the queued marker: doing so would clear
// context before the scheduled prompt ever ran, and release --exclusive while
// the job was still waiting.
func TestCronTurnCorrelation_UnrelatedTurnDoesNotConsumeQueuedMarker(t *testing.T) {
	h := setupPostrunHarness(t)
	jobPrompt := "run your work tick"
	h.marker("work-tick", "started_at=t0\nclear_context=true\nstate=queued\nprompt_sha256="+sha256Hex(jobPrompt)+"\n")

	// The agent was answering something else; that turn ends now.
	if _, code := h.run(); code != 0 {
		t.Fatalf("hook must always exit 0, got %d", code)
	}

	if inv := h.clearInvocations(); inv != "" {
		t.Errorf("must not clear on an unrelated turn's Stop, got %q", inv)
	}
	if _, err := os.Stat(filepath.Join(h.pendDir, "work-tick")); err != nil {
		t.Fatalf("queued marker must survive an unrelated turn: %v", err)
	}
	if !strings.Contains(h.markerBody("work-tick"), "state=queued") {
		t.Errorf("marker must still be queued, got %q", h.markerBody("work-tick"))
	}
}

// The full ordering: queued behind other work, armed when its own prompt is
// finally submitted, then consumed by THAT turn's Stop.
func TestCronTurnCorrelation_ArmsThenClearsOnItsOwnTurn(t *testing.T) {
	h := setupPostrunHarness(t)
	jobPrompt := "run your work tick"
	h.marker("work-tick", "started_at=t0\nclear_context=true\nstate=queued\nprompt_sha256="+sha256Hex(jobPrompt)+"\n")

	// An unrelated turn ends first — marker survives.
	h.run()
	if _, err := os.Stat(filepath.Join(h.pendDir, "work-tick")); err != nil {
		t.Fatalf("queued marker must survive: %v", err)
	}

	// Now the scheduled prompt actually runs.
	if _, code := h.runTurnStart(jobPrompt); code != 0 {
		t.Fatalf("turn-start hook must exit 0, got %d", code)
	}
	if !strings.Contains(h.markerBody("work-tick"), "state=armed") {
		t.Fatalf("marker must be armed by its own prompt, got %q", h.markerBody("work-tick"))
	}

	// And that turn's Stop consumes it.
	if _, code := h.run(); code != 0 {
		t.Fatalf("hook must always exit 0, got %d", code)
	}
	if inv := h.clearInvocations(); !strings.Contains(inv, "/clear") {
		t.Errorf("want the clear on the scheduled prompt's own turn, got %q", inv)
	}
	if _, err := os.Stat(filepath.Join(h.pendDir, "work-tick")); !os.IsNotExist(err) {
		t.Errorf("marker must be removed after its own turn (err=%v)", err)
	}
}

// A prompt the operator typed must not arm a queued cron marker — that is the
// same mis-correlation wearing a different hat.
func TestCronTurnCorrelation_UnrelatedPromptDoesNotArm(t *testing.T) {
	h := setupPostrunHarness(t)
	h.marker("work-tick", "started_at=t0\nclear_context=true\nstate=queued\nprompt_sha256="+sha256Hex("run your work tick")+"\n")

	if _, code := h.runTurnStart("hey what's the status of the deploy?"); code != 0 {
		t.Fatalf("turn-start hook must exit 0, got %d", code)
	}

	if !strings.Contains(h.markerBody("work-tick"), "state=queued") {
		t.Errorf("an unrelated prompt must not arm the marker, got %q", h.markerBody("work-tick"))
	}
}

// Two jobs sharing a prompt must not both arm off one submission — one prompt
// starts one turn.
func TestCronTurnCorrelation_ArmsOnlyOneMarkerPerSubmission(t *testing.T) {
	h := setupPostrunHarness(t)
	p := "do the thing"
	h.marker("job-a", "state=queued\nclear_context=true\nprompt_sha256="+sha256Hex(p)+"\n")
	h.marker("job-b", "state=queued\nclear_context=true\nprompt_sha256="+sha256Hex(p)+"\n")

	if _, code := h.runTurnStart(p); code != 0 {
		t.Fatalf("turn-start hook must exit 0, got %d", code)
	}

	armed := 0
	for _, j := range []string{"job-a", "job-b"} {
		if strings.Contains(h.markerBody(j), "state=armed") {
			armed++
		}
	}
	if armed != 1 {
		t.Errorf("want exactly 1 armed marker, got %d", armed)
	}
}
