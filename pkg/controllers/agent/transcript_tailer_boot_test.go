package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestTranscriptTailer_DoesNotExitWhenProjectsDirAbsent is the kyber#575
// boot-tolerance regression guard. AC7's live production run caught the
// transcript-tailer crash-looping at boot (restartCount climbing to 3): as a
// native sidecar (restartPolicy:Always) it starts AHEAD of the agent container,
// so on a fresh agent neither transcript projects root exists yet, and any
// startup exit is restarted by the kubelet — a visible crash-loop until the agent
// finally writes a transcript. A native sidecar MUST come up at restartCount 0.
//
// This runs the ACTUAL tailer script and asserts it does NOT exit on its own
// while both projects roots are absent — it must wait, not die.
func TestTranscriptTailer_DoesNotExitWhenProjectsDirAbsent(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on the test host")
	}
	// The script's projects roots live under transcriptMountPath (/agent-home),
	// the read-only persist PVC mount. That path does not exist on the test host,
	// so BOTH roots are absent — exactly the fresh-agent boot condition the AC7
	// crash-loop reproduces.
	if _, err := os.Stat(transcriptMountPath); err == nil {
		t.Skipf("%s exists on the test host; cannot simulate absent projects dirs", transcriptMountPath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", transcriptTailScript)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // returns when the context kills it, or earlier if it self-exits

	// The ONLY acceptable end is the context deadline killing a STILL-RUNNING
	// process. If the script exited on its own, ctx.Err() is nil here — and as a
	// native sidecar that exit would crash-loop the container at boot.
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("transcript-tailer exited on its own with absent projects dirs "+
			"(native-sidecar boot crash-loop, kyber#575 AC7). stderr:\n%s", stderr.String())
	}

	// And it must be DELIBERATELY waiting on the projects dir — not coincidentally
	// looping — so the boot-tolerance is explicit and observable in the pod logs.
	if !strings.Contains(stderr.String(), "waiting for a transcript projects dir") {
		t.Errorf("expected the explicit boot-wait gate to log while the projects dir "+
			"is absent; stderr:\n%s", stderr.String())
	}
}

// TestTranscriptTailer_ProceedsOnceProjectsDirExists confirms the boot-wait gate
// does NOT block an agent whose projects dir already exists (a recreated agent
// with a reused PVC, or one that has already written transcripts): the gate must
// pass through immediately. We point the script at a temp dir that DOES contain
// the bind-root layout and assert it gets past the gate (no boot-wait log) and
// reaches the tail loop.
func TestTranscriptTailer_ProceedsOnceProjectsDirExists(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on the test host")
	}
	// Build a tail script whose roots point at a temp dir that exists, so the
	// boot-wait gate is satisfied immediately. We reuse the real script body but
	// override the two root vars + OFFSET_DIR via a prelude (the script reads them
	// as plain shell vars after its own assignments, so the last assignment wins).
	root := t.TempDir()
	projects := root + "/projects"
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	offsets := root + "/offsets"

	// Re-point the roots by appending overriding assignments after the script's
	// own (the script's `set -u` + later re-assignment is valid shell); then the
	// gate sees BIND_ROOT existing and proceeds.
	override := "\nOVERLAY_ROOT=" + projects + "\nBIND_ROOT=" + projects +
		"\nOFFSET_DIR=" + offsets + "\nmkdir -p \"$OFFSET_DIR\"\n"
	// Insert the override right before the boot-wait gate marker.
	marker := "# Boot-tolerance gate"
	idx := strings.Index(transcriptTailScript, marker)
	if idx < 0 {
		t.Fatalf("boot-tolerance gate marker not found in script")
	}
	patched := transcriptTailScript[:idx] + strings.TrimPrefix(override, "\n") + "\n" + transcriptTailScript[idx:]

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", patched)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()

	// It should still be running (tail loop, killed by ctx) and must NOT have
	// logged the boot-wait — the dir existed, so the gate passed straight through.
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("tailer exited unexpectedly with an existing projects dir. stderr:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "waiting for a transcript projects dir") {
		t.Errorf("boot-wait gate must pass through immediately when the projects dir "+
			"already exists; got a wait log. stderr:\n%s", stderr.String())
	}
}
