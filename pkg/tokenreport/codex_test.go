package tokenreport

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCodexLatest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":900,"cached_input_tokens":500,"output_tokens":750,"reasoning_output_tokens":200},"last_token_usage":{"input_tokens":100,"cached_input_tokens":60,"cache_write_input_tokens":4,"output_tokens":30,"reasoning_output_tokens":12},"model_context_window":258400}}}` + "\n"
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := ParseCodexLatest(p, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	// Output must be the CUMULATIVE total_token_usage.output_tokens (750) —
	// not last_token_usage's 30 (which loses intermediate messages between
	// polls) and not plus reasoning_output_tokens (a subset of output_tokens
	// that would double-count).
	if s == nil || s.Tokens.Used != 100 || s.Tokens.Input != 40 || s.Tokens.CacheRead != 60 || s.Tokens.Output != 750 || s.Tokens.Limit != 258400 {
		t.Fatalf("snapshot = %+v", s)
	}
	if !s.ContextWindowKnown || s.Percentage <= 0 {
		t.Fatalf("context metadata = known:%v percentage:%v", s.ContextWindowKnown, s.Percentage)
	}
}

func TestDetectCodexActivityAcrossRollouts(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "old.jsonl")
	newer := filepath.Join(root, "new.jsonl")
	if err := os.WriteFile(old, []byte(
		`{"timestamp":"2026-08-03T10:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`+"\n"+
			`{"timestamp":"2026-08-03T10:00:05Z","type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A newer empty rollout must not hide the previous completed task.
	if err := os.WriteFile(newer, []byte(`{"timestamp":"2026-08-03T11:00:00Z","type":"session_meta","payload":{"session_id":"new"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state, at, err := DetectCodexActivity(root)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 3, 10, 0, 5, 0, time.UTC)
	if state != ActivityIdle || !at.Equal(want) {
		t.Fatalf("activity = %q at %s, want idle at %s", state, at, want)
	}
}
