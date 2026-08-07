package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// These tests EXECUTE the embedded transcriptTailScript against real on-disk
// fixtures (kyber#584). The unit tests above assert the script's TEXT; these
// assert its BEHAVIOR — that the single-process incremental reader ships active
// sessions exactly once and in order, skips idle ones (active-set bounding), and
// stays bounded over a ≥5,000-file backlog. The script is parameterized via the
// TRANSCRIPT_* env vars (overlay/bind roots, offset dir, poll cadence, and a
// test-only bounded poll count) so it can run against a temp tree without the
// in-cluster PVC mounts.

// requireScriptTools skips the test when a tool the script needs isn't present
// (e.g., a minimal CI image without mawk). The production agent runtime image is
// Ubuntu and has all of these; this keeps the suite green where they're absent
// while still giving real behavioral proof where they exist.
func requireScriptTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("transcript-tailer script is a Linux sidecar; skipping on %s", runtime.GOOS)
	}
	for _, tool := range []string{"bash", "mawk", "md5sum", "stat", "wc", "find"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("required tool %q not on PATH; skipping executable script test", tool)
		}
	}
}

// scriptHarness runs the embedded tailer script over a fixture tree for a bounded
// number of polls and returns everything it shipped to stdout (the lines Vector
// would route to the transcript lane).
type scriptHarness struct {
	overlayRoot string // the session-file tree the script scans
	offsetDir   string // durable checkpoint dir (kyber#467 PVC stand-in)
}

func newScriptHarness(t *testing.T) *scriptHarness {
	t.Helper()
	requireScriptTools(t)
	dir := t.TempDir()
	h := &scriptHarness{
		overlayRoot: filepath.Join(dir, "projects"),
		offsetDir:   filepath.Join(dir, "offsets"),
	}
	if err := os.MkdirAll(h.overlayRoot, 0o755); err != nil {
		t.Fatalf("mkdir overlay root: %v", err)
	}
	if err := os.MkdirAll(h.offsetDir, 0o755); err != nil {
		t.Fatalf("mkdir offset dir: %v", err)
	}
	return h
}

// sessionFile returns the path to a named *.jsonl session file under the tree.
func (h *scriptHarness) sessionFile(name string) string {
	return filepath.Join(h.overlayRoot, name)
}

// appendLines appends newline-terminated lines to a session file (append-only,
// like Claude Code writing a transcript).
func (h *scriptHarness) appendLines(t *testing.T, name string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(h.sessionFile(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	for _, ln := range lines {
		if _, err := f.WriteString(ln + "\n"); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// runPolls executes the script for exactly n polls and returns the shipped
// stdout lines. The bind root is pointed at a non-existent path (only the
// overlay tree is used). POLL_SECONDS=0 makes the bounded run finish promptly.
func (h *scriptHarness) runPolls(t *testing.T, n int) []string {
	t.Helper()
	cmd := exec.Command("bash", "-c", transcriptTailScript)
	cmd.Env = append(os.Environ(),
		"TRANSCRIPT_OVERLAY_ROOT="+h.overlayRoot,
		"TRANSCRIPT_BIND_ROOT="+filepath.Join(t.TempDir(), "nonexistent-bind"),
		"TRANSCRIPT_OFFSET_DIR="+h.offsetDir,
		"TRANSCRIPT_POLL_SECONDS=0",
		"TRANSCRIPT_POLL_LIMIT="+strconv.Itoa(n),
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script run failed: %v\nstderr/out: %s", err, string(out))
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan shipped output: %v", err)
	}
	return lines
}

// TestTranscriptTailerScript_ShipsActiveFileInOrder is the baseline behavior: a
// fresh session file with no checkpoint ships in full, in order, exactly once.
func TestTranscriptTailerScript_ShipsActiveFileInOrder(t *testing.T) {
	h := newScriptHarness(t)
	h.appendLines(t, "sess-a.jsonl", `{"i":1}`, `{"i":2}`, `{"i":3}`)

	got := h.runPolls(t, 1)

	want := []string{`{"i":1}`, `{"i":2}`, `{"i":3}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("shipped lines = %q, want %q (in order, exactly once)", got, want)
	}
}

// TestTranscriptTailerScript_IdleFileNotReshipped is the kyber#584 Phase A AC
// AND the exactly-once invariant: once a file is fully shipped, a subsequent
// poll with no new content ships NOTHING (the file is idle — not held in any live
// tail set, not re-read), and a fresh process (sidecar restart / pod recreation)
// resuming from the durable checkpoint also re-ships nothing.
func TestTranscriptTailerScript_IdleFileNotReshipped(t *testing.T) {
	h := newScriptHarness(t)
	h.appendLines(t, "sess-a.jsonl", `{"i":1}`, `{"i":2}`)

	if got := h.runPolls(t, 1); len(got) != 2 {
		t.Fatalf("first poll shipped %d lines, want 2: %q", len(got), got)
	}
	// Second poll, same process lifetime would be covered by a 2-poll run; here
	// we start a BRAND NEW process (simulating a sidecar restart) against the
	// same durable offset dir. Exactly-once must hold across the restart.
	if got := h.runPolls(t, 1); len(got) != 0 {
		t.Errorf("idle file re-shipped %d lines after restart, want 0 (exactly-once): %q", len(got), got)
	}
}

// TestTranscriptTailerScript_ReadmitsOnGrowth is the kyber#584 Phase A
// re-admit-on-growth AC: an idle (fully-shipped) file that grows again is picked
// back up and only its NEW lines ship — no gap, no duplicate.
func TestTranscriptTailerScript_ReadmitsOnGrowth(t *testing.T) {
	h := newScriptHarness(t)
	h.appendLines(t, "sess-a.jsonl", `{"i":1}`, `{"i":2}`)
	if got := h.runPolls(t, 1); len(got) != 2 {
		t.Fatalf("first poll shipped %d, want 2: %q", len(got), got)
	}

	// File grows (new process, like a later reconcile/restart).
	h.appendLines(t, "sess-a.jsonl", `{"i":3}`, `{"i":4}`)
	got := h.runPolls(t, 1)
	want := []string{`{"i":3}`, `{"i":4}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("after growth shipped %q, want only the new lines %q", got, want)
	}
}

// TestTranscriptTailerScript_LargeFileResumeShipsAllLines is the regression guard
// for the mawk `-W interactive` strand bug. A session file shipped once and then
// grown LARGE must, on RESUME (start>1), ship EVERY new complete line — no gap.
// The bug (mawk 1.3.4 `-W interactive` + the NR<s{next} resume rule) shipped only
// a truncated prefix of the new lines once the file exceeded mawk's interactive
// read buffer, stranding the rest; the unconditional size checkpoint then skipped
// the file forever. The fixture is deliberately large (~160 KB) so the strand
// reproduces on the affected mawk; on an unaffected mawk it still guards the
// no-gap-on-resume invariant. Distinct from ReadmitsOnGrowth, whose tiny fixture
// fits in one buffer and never triggered the bug.
func TestTranscriptTailerScript_LargeFileResumeShipsAllLines(t *testing.T) {
	h := newScriptHarness(t)

	// First ship: a single line (the start=1 path, which never triggered the bug).
	h.appendLines(t, "sess-a.jsonl", `{"i":0}`)
	if got := h.runPolls(t, 1); len(got) != 1 {
		t.Fatalf("first poll shipped %d, want 1: %q", len(got), got)
	}

	// Grow the file large: n padded lines so the total comfortably exceeds any
	// plausible interactive read buffer.
	const n = 800
	pad := strings.Repeat("x", 180)
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		lines[i] = fmt.Sprintf(`{"i":%d,"pad":%q}`, i+1, pad)
	}
	h.appendLines(t, "sess-a.jsonl", lines...)

	// Resume (start=2): every one of the n new complete lines must ship, in order.
	got := h.runPolls(t, 1)
	if len(got) != n {
		t.Fatalf("resume shipped %d lines, want %d — a truncated resume is the `-W interactive` strand bug", len(got), n)
	}
	if got[0] != lines[0] || got[n-1] != lines[n-1] {
		t.Errorf("resume shipped out of order or truncated: first=%q last=%q want first=%q last=%q",
			got[0], got[n-1], lines[0], lines[n-1])
	}
}

// TestTranscriptTailerScript_StaleCheckpointClampReships is the kyber#467 safety
// invariant preserved across the rewrite: a durable checkpoint pointing past a
// file that was truncated/rotated (now shorter) must NOT silently skip the new,
// shorter content — the script clamps and re-ships from line 1.
func TestTranscriptTailerScript_StaleCheckpointClampReships(t *testing.T) {
	h := newScriptHarness(t)
	h.appendLines(t, "sess-a.jsonl", `{"i":1}`, `{"i":2}`, `{"i":3}`, `{"i":4}`, `{"i":5}`)
	if got := h.runPolls(t, 1); len(got) != 5 {
		t.Fatalf("first poll shipped %d, want 5: %q", len(got), got)
	}

	// Truncate + rewrite shorter (rotation out of band): checkpoint now points
	// past EOF. The clamp must re-ship the whole new content from line 1.
	if err := os.WriteFile(h.sessionFile("sess-a.jsonl"), []byte("{\"r\":1}\n{\"r\":2}\n"), 0o644); err != nil {
		t.Fatalf("truncate-rewrite: %v", err)
	}
	got := h.runPolls(t, 1)
	want := []string{`{"r":1}`, `{"r":2}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("after truncation shipped %q, want the clamped re-ship %q (no silent skip)", got, want)
	}
}

// TestTranscriptTailerScript_PartialTrailingLineNotShipped guards audit
// correctness: a half-written final line (no newline yet) must NOT be shipped or
// checkpointed; it ships on a later poll once its newline lands — never partial,
// never skipped.
func TestTranscriptTailerScript_PartialTrailingLineNotShipped(t *testing.T) {
	h := newScriptHarness(t)
	// Two complete lines + a partial third (no trailing newline).
	if err := os.WriteFile(h.sessionFile("sess-a.jsonl"), []byte("{\"i\":1}\n{\"i\":2}\n{\"par"), 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	got := h.runPolls(t, 1)
	if want := []string{`{"i":1}`, `{"i":2}`}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("shipped %q, want only the 2 complete lines %q (partial line withheld)", got, want)
	}

	// Complete the third line; it must now ship exactly once, no dup of 1/2.
	if err := os.WriteFile(h.sessionFile("sess-a.jsonl"), []byte("{\"i\":1}\n{\"i\":2}\n{\"par\":3}\n"), 0o644); err != nil {
		t.Fatalf("complete line: %v", err)
	}
	got = h.runPolls(t, 1)
	if want := []string{`{"par":3}`}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("after completion shipped %q, want only the now-complete line %q", got, want)
	}
}

// TestTranscriptTailerScript_BoundedOverManyFiles is the kyber#584 core AC
// (AC1/AC2): with a large backlog of fully-shipped (aged-checkpoint) session
// files plus one active session, a poll ships ONLY the active session's new
// lines and does NOT re-read or re-ship the thousands of idle files. This is the
// behavioral stand-in for the ≥5,000-file memory invariant: the work (and thus
// memory) a poll does is bounded by the ACTIVE set, not the historical file
// count. Run with a real ≥5,000-file fixture in -short=false; capped smaller in
// -short to keep the unit suite fast.
func TestTranscriptTailerScript_BoundedOverManyFiles(t *testing.T) {
	h := newScriptHarness(t)

	nFiles := 5000
	if testing.Short() {
		nFiles = 200
	}

	// Seed nFiles aged session files and ship them once so each gets a durable
	// checkpoint at EOF (the "aged-offset checkpoint set" the AC describes).
	for i := 0; i < nFiles; i++ {
		h.appendLines(t, fmt.Sprintf("aged-%05d.jsonl", i), fmt.Sprintf(`{"f":%d}`, i))
	}
	if got := h.runPolls(t, 1); len(got) != nFiles {
		t.Fatalf("seed poll shipped %d, want %d (one line per aged file)", len(got), nFiles)
	}

	// Now ONE active session grows. A poll over the whole tree must ship ONLY its
	// new lines — the nFiles idle files are skipped (size unchanged), not re-shipped.
	h.appendLines(t, "active.jsonl", `{"live":1}`, `{"live":2}`)
	got := h.runPolls(t, 1)
	want := []string{`{"live":1}`, `{"live":2}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("poll over %d idle files + 1 active shipped %q, want only the active session's new lines %q "+
			"(idle files must not be re-shipped — active-set bounding)", nFiles, got, want)
	}
}

// TestTranscriptTailerScript_BothRootsScanned confirms a session file under the
// BIND-mount fallback root is shipped too (not only the overlay root).
func TestTranscriptTailerScript_BothRootsScanned(t *testing.T) {
	requireScriptTools(t)
	dir := t.TempDir()
	overlay := filepath.Join(dir, "overlay")
	bind := filepath.Join(dir, "bind")
	offsets := filepath.Join(dir, "offsets")
	for _, d := range []string{overlay, bind, offsets} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(bind, "b.jsonl"), []byte("{\"from\":\"bind\"}\n"), 0o644); err != nil {
		t.Fatalf("write bind file: %v", err)
	}

	cmd := exec.Command("bash", "-c", transcriptTailScript)
	cmd.Env = append(os.Environ(),
		"TRANSCRIPT_OVERLAY_ROOT="+overlay,
		"TRANSCRIPT_BIND_ROOT="+bind,
		"TRANSCRIPT_OFFSET_DIR="+offsets,
		"TRANSCRIPT_POLL_SECONDS=0",
		"TRANSCRIPT_POLL_LIMIT=1",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script run failed: %v\nout: %s", err, string(out))
	}
	if !strings.Contains(string(out), `{"from":"bind"}`) {
		t.Errorf("bind-root session file was not shipped; out=%q", string(out))
	}
}
