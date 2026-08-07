package tokenreport_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

func TestParseLatest_FinalizedMessage(t *testing.T) {
	snap, err := tokenreport.ParseLatest("testdata", "finalized.jsonl")
	if err != nil {
		t.Fatalf("ParseLatest: %v", err)
	}
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if snap.Model != "claude-sonnet-4-5" {
		t.Errorf("Model=%q want %q", snap.Model, "claude-sonnet-4-5")
	}
	if snap.Tokens.Used != 45231 {
		t.Errorf("Tokens.Used=%d want 45231", snap.Tokens.Used)
	}
	// #396: the in-pod reporter no longer computes limit/pct — the limit is
	// resolved server-side at serve-time from the operator ConfigMap. The
	// parser emits Limit=0 / Percentage=0 as the "resolve upstream" sentinel.
	if snap.Tokens.Limit != 0 {
		t.Errorf("Tokens.Limit=%d want 0 (server resolves the limit, #396)", snap.Tokens.Limit)
	}
	if snap.Tokens.Input != 12000 {
		t.Errorf("Tokens.Input=%d want 12000", snap.Tokens.Input)
	}
	if snap.Tokens.CacheCreation != 8000 {
		t.Errorf("Tokens.CacheCreation=%d want 8000", snap.Tokens.CacheCreation)
	}
	if snap.Tokens.CacheRead != 25231 {
		t.Errorf("Tokens.CacheRead=%d want 25231", snap.Tokens.CacheRead)
	}
	// ParseLatest deliberately leaves Output at 0: a single latest message
	// cannot represent cumulative spend (intermediate messages between
	// reporter ticks would be lost). The Reporter's outputTracker owns the
	// cumulative total and overwrites Tokens.Output before POSTing.
	if snap.Tokens.Output != 0 {
		t.Errorf("Tokens.Output=%d want 0 (cumulative output is the reporter's outputTracker's job)", snap.Tokens.Output)
	}
	// Output is billed spend, NOT part of the context window: Used must
	// stay Input + CacheCreation + CacheRead and never include Output.
	if snap.Tokens.Used != snap.Tokens.Input+snap.Tokens.CacheCreation+snap.Tokens.CacheRead {
		t.Errorf("Tokens.Used=%d must equal Input+CacheCreation+CacheRead (Output excluded)", snap.Tokens.Used)
	}
	if snap.EffortLevel != "medium" {
		t.Errorf("EffortLevel=%q want medium", snap.EffortLevel)
	}
	if snap.Speed != "standard" {
		t.Errorf("Speed=%q want standard", snap.Speed)
	}
	if snap.Percentage != 0 {
		t.Errorf("Percentage=%f want 0 (server computes pct at serve-time, #396)", snap.Percentage)
	}
}

func TestParseLatest_StreamingMessageReturnsNil(t *testing.T) {
	snap, err := tokenreport.ParseLatest("testdata", "streaming.jsonl")
	if err != nil {
		t.Fatalf("ParseLatest: %v", err)
	}
	if snap != nil {
		t.Errorf("expected nil snapshot (no finalized message), got %+v", snap)
	}
}

func TestParseLatest_EmptyFileReturnsNil(t *testing.T) {
	snap, err := tokenreport.ParseLatest("testdata", "empty.jsonl")
	if err != nil {
		t.Fatalf("ParseLatest: %v", err)
	}
	if snap != nil {
		t.Errorf("expected nil snapshot, got %+v", snap)
	}
}

func TestParseLatest_MissingFile(t *testing.T) {
	_, err := tokenreport.ParseLatest("testdata", "does-not-exist.jsonl")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFindLatestSessionFile_PicksNewest(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "new.jsonl")
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the new file's mtime strictly newer.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(newPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := tokenreport.FindLatestSessionFile(dir)
	if err != nil {
		t.Fatalf("FindLatestSessionFile: %v", err)
	}
	if got != newPath {
		t.Errorf("got %q want %q", got, newPath)
	}
}

func TestFindLatestSessionFile_EmptyDirReturnsError(t *testing.T) {
	dir := t.TempDir()
	_, err := tokenreport.FindLatestSessionFile(dir)
	if err == nil {
		t.Fatal("expected error for empty dir")
	}
}
