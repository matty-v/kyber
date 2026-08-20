package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverClaudeModelsUsesOAuthCredentials(t *testing.T) {
	t.Setenv("CLAUDE_ACCESS_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	creds := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"oauth-token"}}`), 0o600); err != nil {
		t.Fatalf("writing credentials: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-1","display_name":"Claude Opus","max_input_tokens":200000}]}`))
	}))
	defer server.Close()

	models, err := discoverClaudeModels(context.Background(), creds, server.URL, server.Client())
	if err != nil {
		t.Fatalf("discoverClaudeModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "claude-opus-4-1" || !models[0].ContextWindowKnown {
		t.Fatalf("models = %+v", models)
	}
}

func TestDiscoverClaudeModelsRejectsMissingContextWindow(t *testing.T) {
	t.Setenv("CLAUDE_ACCESS_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "api-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet","display_name":"Claude Sonnet"}]}`))
	}))
	defer server.Close()

	if _, err := discoverClaudeModels(context.Background(), filepath.Join(t.TempDir(), "missing"), server.URL, server.Client()); err == nil {
		t.Fatal("discoverClaudeModels returned nil error without max_input_tokens")
	}
}

func TestDiscoverClaudeModelsRequiresAuthentication(t *testing.T) {
	t.Setenv("CLAUDE_ACCESS_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := discoverClaudeModels(context.Background(), filepath.Join(t.TempDir(), "missing"), "http://unused", http.DefaultClient); err == nil {
		t.Fatal("discoverClaudeModels returned nil error without credentials")
	}
}

func TestReportClaudeModelCatalogPostsCachedMetadata(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/runtime-catalog" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body struct {
			Runtime string         `json:"runtime"`
			Models  []catalogModel `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		if body.Runtime != "claude-code" || len(body.Models) != 1 || body.Models[0].ID != "claude-sonnet-5" {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	reportClaudeModelCatalog(context.Background(), server.URL, []catalogModel{{ID: "claude-sonnet-5"}}, server.Client())
	if !called {
		t.Fatal("catalog was not posted")
	}
}

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
