package tokenreport

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// assistantLine builds a finalized assistant transcript line with the given
// message id and output token count.
func assistantLine(msgID string, output int64) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"line-%s","message":{"id":%q,"model":"claude-sonnet-4-6","role":"assistant","usage":{"input_tokens":100,"output_tokens":%d,"speed":"standard"}}}`+"\n", msgID, msgID, output)
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// TestOutputTracker_MultiMessagePerTick is the sampling-undercount regression
// gate: several finalized assistant messages appended between two ticks must
// ALL be counted. The pre-fix code read only the newest finalized message per
// tick, permanently losing every intermediate message's output.
func TestOutputTracker_MultiMessagePerTick(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	appendFile(t, path, assistantLine("msg_historical", 999))

	tr := newOutputTracker()
	if got := tr.advance(path); got != 0 {
		t.Fatalf("initial advance = %d, want 0 (historical content is skipped)", got)
	}

	// Three messages land in one tick window.
	appendFile(t, path, assistantLine("msg_a", 10)+assistantLine("msg_b", 20)+assistantLine("msg_c", 30))
	if got := tr.advance(path); got != 60 {
		t.Fatalf("advance after 3 messages = %d, want 60 (10+20+30 — intermediate messages must not be lost)", got)
	}

	// Idle tick: nothing appended → total unchanged.
	if got := tr.advance(path); got != 60 {
		t.Fatalf("idle advance = %d, want 60", got)
	}
}

// TestOutputTracker_DedupByMessageID: multi-block messages repeat the same
// message.id (and usage) across several JSONL lines — count once.
func TestOutputTracker_DedupByMessageID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	appendFile(t, path, "")

	tr := newOutputTracker()
	_ = tr.advance(path)

	appendFile(t, path, assistantLine("msg_x", 40)+assistantLine("msg_x", 40)+assistantLine("msg_y", 5))
	if got := tr.advance(path); got != 45 {
		t.Fatalf("advance = %d, want 45 (msg_x counted once despite two lines)", got)
	}
}

// TestOutputTracker_SkipsStreamingAndNonAssistant: streaming entries
// (speed "?" / absent) and user lines never count.
func TestOutputTracker_SkipsStreamingAndNonAssistant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	appendFile(t, path, "")

	tr := newOutputTracker()
	_ = tr.advance(path)

	appendFile(t, path,
		`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"+
			`{"type":"assistant","message":{"id":"msg_s","usage":{"output_tokens":50,"speed":"?"}}}`+"\n"+
			`{"type":"assistant","message":{"id":"msg_n","usage":{"output_tokens":50}}}`+"\n"+
			assistantLine("msg_ok", 7))
	if got := tr.advance(path); got != 7 {
		t.Fatalf("advance = %d, want 7 (only the finalized assistant line counts)", got)
	}
}

// TestOutputTracker_RotationDrainsPreviousFile: when the newest session file
// changes (Claude Code rotates on /clear), messages appended to the OLD file
// since the last read must still be counted before switching.
func TestOutputTracker_RotationDrainsPreviousFile(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.jsonl")
	fileB := filepath.Join(dir, "b.jsonl")
	appendFile(t, fileA, "")

	tr := newOutputTracker()
	_ = tr.advance(fileA)

	// A message lands in A, then the session rotates to B with its own message.
	appendFile(t, fileA, assistantLine("msg_a_tail", 11))
	appendFile(t, fileB, assistantLine("msg_b_first", 22))

	if got := tr.advance(fileB); got != 33 {
		t.Fatalf("advance after rotation = %d, want 33 (A's tail drained + B counted from 0)", got)
	}
}

// TestOutputTracker_IncompleteTrailingLine: a line without a trailing newline
// (mid-append) is left for the next tick, then counted exactly once when
// completed.
func TestOutputTracker_IncompleteTrailingLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	appendFile(t, path, "")

	tr := newOutputTracker()
	_ = tr.advance(path)

	full := assistantLine("msg_partial", 13)
	appendFile(t, path, full[:len(full)/2])
	if got := tr.advance(path); got != 0 {
		t.Fatalf("advance with half-written line = %d, want 0", got)
	}
	appendFile(t, path, full[len(full)/2:])
	if got := tr.advance(path); got != 13 {
		t.Fatalf("advance after line completed = %d, want 13", got)
	}
}

// TestOutputTracker_NegativeOutputIgnored: a malformed negative count never
// decrements the running total.
func TestOutputTracker_NegativeOutputIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	appendFile(t, path, "")

	tr := newOutputTracker()
	_ = tr.advance(path)

	appendFile(t, path, assistantLine("msg_good", 9)+assistantLine("msg_bad", -50))
	if got := tr.advance(path); got != 9 {
		t.Fatalf("advance = %d, want 9 (negative output ignored)", got)
	}
}

// TestOutputTracker_DedupFallsBackToUUID: lines without a message.id dedup on
// the transcript uuid instead.
func TestOutputTracker_DedupFallsBackToUUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	appendFile(t, path, "")

	tr := newOutputTracker()
	_ = tr.advance(path)

	line := `{"type":"assistant","uuid":"u-1","message":{"usage":{"output_tokens":15,"speed":"standard"}}}` + "\n"
	appendFile(t, path, line+line)
	if got := tr.advance(path); got != 15 {
		t.Fatalf("advance = %d, want 15 (uuid dedup)", got)
	}
}
