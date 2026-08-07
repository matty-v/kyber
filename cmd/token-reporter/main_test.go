package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPickLatestSubdir_IgnoresDirWithoutJSONL reproduces the bug that
// left agents showing "No token data yet" in the UI: a sibling project
// dir with a newer mtime but no .jsonl files would steal the pick from
// the actively-written session dir.
func TestPickLatestSubdir_IgnoresDirWithoutJSONL(t *testing.T) {
	root := t.TempDir()

	active := filepath.Join(root, "-")
	decoy := filepath.Join(root, "-home-kyber-dev-chewie-agent")
	for _, d := range []string{active, decoy} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Active dir contains a real session .jsonl.
	jsonl := filepath.Join(active, "session.jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	// Backdate the .jsonl so the decoy's dir mtime is strictly newer than
	// both the active dir and its session file. Under the old mtime-based
	// pick this misdirected the reporter.
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(jsonl, past, past); err != nil {
		t.Fatalf("chtimes jsonl: %v", err)
	}
	if err := os.Chtimes(active, past, past); err != nil {
		t.Fatalf("chtimes active: %v", err)
	}
	future := time.Now()
	if err := os.Chtimes(decoy, future, future); err != nil {
		t.Fatalf("chtimes decoy: %v", err)
	}

	got := pickLatestSubdir(root)
	if got != active {
		t.Fatalf("pickLatestSubdir = %q, want %q (decoy has newer dir mtime but no .jsonl)", got, active)
	}
}

// TestPickLatestSubdir_NewestJSONLWins confirms the pick follows the
// freshest session across dirs that both contain .jsonl files.
func TestPickLatestSubdir_NewestJSONLWins(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	older := filepath.Join(a, "s.jsonl")
	newer := filepath.Join(b, "s.jsonl")
	for _, p := range []string{older, newer} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatalf("chtimes older: %v", err)
	}

	got := pickLatestSubdir(root)
	if got != b {
		t.Fatalf("pickLatestSubdir = %q, want %q", got, b)
	}
}

// TestPickLatestSubdir_EmptyRoot returns the empty string when the
// projects root has no subdirs yet (fresh pod, no session started).
func TestPickLatestSubdir_EmptyRoot(t *testing.T) {
	if got := pickLatestSubdir(t.TempDir()); got != "" {
		t.Fatalf("pickLatestSubdir on empty = %q, want \"\"", got)
	}
}

// TestPickLatestSubdir_AllDirsEmpty returns "" when subdirs exist but
// none contain .jsonl files — the old implementation would happily return
// one of them, leading to the reporter polling an empty dir.
func TestPickLatestSubdir_AllDirsEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := pickLatestSubdir(root); got != "" {
		t.Fatalf("pickLatestSubdir on dirs-without-jsonl = %q, want \"\"", got)
	}
}
