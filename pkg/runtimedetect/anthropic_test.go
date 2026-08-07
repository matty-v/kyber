package runtimedetect_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

const anthropicFixture = `{
  "data": [
    {"id": "claude-opus-4-7", "type": "model", "display_name": "Claude Opus 4.7", "created_at": "2026-05-01T00:00:00Z"},
    {"id": "claude-sonnet-4-6", "type": "model", "display_name": "Claude Sonnet 4.6", "created_at": "2026-04-15T00:00:00Z"}
  ],
  "has_more": false
}`

func TestAnthropicClient_Fetch_ParsesModels(t *testing.T) {
	var gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicFixture))
	}))
	defer srv.Close()

	c := runtimedetect.NewAnthropicClient(srv.URL, "", 5*time.Second)
	got, err := c.Fetch(context.Background(), "sk-test-12345")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotKey != "sk-test-12345" {
		t.Fatalf("expected x-api-key forwarded, got %q", gotKey)
	}
	if gotVersion == "" {
		t.Fatalf("expected anthropic-version header set, got empty")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(got), got)
	}
	if got[0].ID != "claude-opus-4-7" || got[0].DisplayName != "Claude Opus 4.7" {
		t.Fatalf("first model wrong: %+v", got[0])
	}
	// PR-A: every model defaults to 200K floor + ContextWindowKnown=false.
	if got[0].ContextWindow != runtimedetect.DefaultContextWindowFloor {
		t.Fatalf("expected 200K floor, got %d", got[0].ContextWindow)
	}
	if got[0].ContextWindowKnown {
		t.Fatalf("expected ContextWindowKnown=false in PR-A, got true")
	}
}

// anthropicFixtureWithWindows mirrors the real Models API list response,
// which carries max_input_tokens per model object (kyber#488). opus exposes
// the field; the second entry omits it to exercise the absent-field fallback.
const anthropicFixtureWithWindows = `{
  "data": [
    {"id": "claude-opus-4-8", "type": "model", "display_name": "Claude Opus 4.8", "created_at": "2026-06-01T00:00:00Z", "max_input_tokens": 1000000},
    {"id": "claude-legacy-1", "type": "model", "display_name": "Legacy", "created_at": "2025-01-01T00:00:00Z"}
  ],
  "has_more": false
}`

func TestAnthropicClient_Fetch_DecodesMaxInputTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicFixtureWithWindows))
	}))
	defer srv.Close()

	c := runtimedetect.NewAnthropicClient(srv.URL, "", 5*time.Second)
	got, err := c.Fetch(context.Background(), "sk-test")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(got), got)
	}

	// Model with max_input_tokens present: real window, ContextWindowKnown=true.
	if got[0].ID != "claude-opus-4-8" {
		t.Fatalf("first model wrong: %+v", got[0])
	}
	if got[0].ContextWindow != 1_000_000 {
		t.Errorf("opus window: got %d, want 1000000 (from max_input_tokens)", got[0].ContextWindow)
	}
	if !got[0].ContextWindowKnown {
		t.Errorf("opus ContextWindowKnown: got false, want true (max_input_tokens present)")
	}

	// Model omitting max_input_tokens: floor + ContextWindowKnown=false.
	if got[1].ContextWindow != runtimedetect.DefaultContextWindowFloor {
		t.Errorf("legacy window: got %d, want %d (floor fallback)", got[1].ContextWindow, runtimedetect.DefaultContextWindowFloor)
	}
	if got[1].ContextWindowKnown {
		t.Errorf("legacy ContextWindowKnown: got true, want false (max_input_tokens absent)")
	}
}

// A zero or negative max_input_tokens is treated as absent — never set a
// model's window to 0 (that would render an absurd >100% gauge for any usage).
func TestAnthropicClient_Fetch_NonPositiveMaxInputTokensTreatedAsAbsent(t *testing.T) {
	const fixture = `{"data":[{"id":"claude-weird","type":"model","display_name":"Weird","max_input_tokens":0}],"has_more":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	c := runtimedetect.NewAnthropicClient(srv.URL, "", 5*time.Second)
	got, err := c.Fetch(context.Background(), "sk-test")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 model, got %d", len(got))
	}
	if got[0].ContextWindow != runtimedetect.DefaultContextWindowFloor || got[0].ContextWindowKnown {
		t.Errorf("max_input_tokens=0 should fall back to floor+false, got (%d, %v)", got[0].ContextWindow, got[0].ContextWindowKnown)
	}
}

func TestAnthropicClient_Fetch_EmptyKeyReturnsTyped(t *testing.T) {
	c := runtimedetect.NewAnthropicClient("http://unused", "", 5*time.Second)
	_, err := c.Fetch(context.Background(), "")
	if !errors.Is(err, runtimedetect.ErrAnthropicKeyMissing) {
		t.Fatalf("expected ErrAnthropicKeyMissing, got %v", err)
	}
}

func TestAnthropicClient_Fetch_401ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := runtimedetect.NewAnthropicClient(srv.URL, "", 5*time.Second)
	_, err := c.Fetch(context.Background(), "sk-bad")
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	// The error message must not echo the key — defensive logging guard.
	if errStr := err.Error(); contains(errStr, "sk-bad") {
		t.Fatalf("error message leaked the api key: %q", errStr)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
