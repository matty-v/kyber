package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

// TestHandleAvailable_EmptyCache_ReturnsContractShape: even when nothing
// has been polled yet, the handler must return the documented contract
// (empty arrays, 200) so the PWA picker can render without an error.
func TestHandleAvailable_EmptyCache_ReturnsContractShape(t *testing.T) {
	s := &Server{RuntimeDetectCache: runtimedetect.NewMemoryCache()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/available", nil)
	rr := httptest.NewRecorder()
	s.handleAvailable(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got AvailableResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClaudeCodeVersions == nil {
		t.Error("expected non-nil claudeCodeVersions array (contract: [] not null)")
	}
	if got.CodexVersions == nil || got.CodexModels == nil {
		t.Error("expected non-nil Codex catalog arrays (contract: [] not null)")
	}
	if got.Models == nil {
		t.Error("expected non-nil models array (contract: [] not null)")
	}
	if len(got.ClaudeCodeVersions) != 0 || len(got.Models) != 0 {
		t.Errorf("expected empty payload on empty cache, got %+v", got)
	}
}

// TestHandleAvailable_NilCache_ReturnsContractShape: when the operator
// disables detection entirely (e.g., runtimedetect.enabled=false in the
// chart), the PWA must still render — no 503, just empty arrays.
func TestHandleAvailable_NilCache_ReturnsContractShape(t *testing.T) {
	s := &Server{RuntimeDetectCache: nil}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/available", nil)
	rr := httptest.NewRecorder()
	s.handleAvailable(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got AvailableResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClaudeCodeVersions == nil || got.Models == nil {
		t.Errorf("expected non-nil arrays even with nil cache, got %+v", got)
	}
	if got.CodexVersions == nil || got.CodexModels == nil {
		t.Errorf("expected non-nil Codex arrays even with nil cache, got %+v", got)
	}
}

// TestHandleAvailable_PinsResponseShape: contract test the PWA depends on.
// Every model entry surfaces id, displayName, contextWindow,
// contextWindowKnown — and PR-A defaults contextWindow to 200K floor with
// contextWindowKnown=false. Removing/renaming any of these is a breaking
// change.
func TestHandleAvailable_PinsResponseShape(t *testing.T) {
	cache := runtimedetect.NewMemoryCache()
	if err := cache.Put(context.Background(), &runtimedetect.Snapshot{
		ClaudeCodeVersions: []string{"2.0.0", "1.5.0"},
		CodexVersions:      []string{"0.146.0"},
		Models: []runtimedetect.Model{
			{ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7", ContextWindow: runtimedetect.DefaultContextWindowFloor, ContextWindowKnown: false},
			{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", ContextWindow: runtimedetect.DefaultContextWindowFloor, ContextWindowKnown: false},
		},
		CodexModels: []runtimedetect.Model{{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6-Sol", ContextWindow: 200_000}},
		FetchedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	s := &Server{RuntimeDetectCache: cache}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/available", nil)
	rr := httptest.NewRecorder()
	s.handleAvailable(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// Parse as a generic map first so we can assert key names directly
	// — this is the PWA contract; renaming a field silently is a
	// regression we want a test to catch.
	var raw struct {
		ClaudeCodeVersions []string                 `json:"claudeCodeVersions"`
		CodexVersions      []string                 `json:"codexVersions"`
		Models             []map[string]interface{} `json:"models"`
		CodexModels        []map[string]interface{} `json:"codexModels"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw.ClaudeCodeVersions) != 2 || raw.ClaudeCodeVersions[0] != "2.0.0" {
		t.Errorf("versions: %+v", raw.ClaudeCodeVersions)
	}
	if len(raw.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(raw.Models))
	}
	if len(raw.CodexVersions) != 1 || len(raw.CodexModels) != 1 || raw.CodexModels[0]["id"] != "gpt-5.6-sol" {
		t.Errorf("Codex catalog not surfaced: versions=%v models=%v", raw.CodexVersions, raw.CodexModels)
	}
	m := raw.Models[0]
	for _, k := range []string{"id", "displayName", "contextWindow", "contextWindowKnown"} {
		if _, ok := m[k]; !ok {
			t.Errorf("model missing field %q: %+v", k, m)
		}
	}
	if m["id"] != "claude-opus-4-7" {
		t.Errorf("model id: %v", m["id"])
	}
	if m["contextWindowKnown"] != false {
		t.Errorf("PR-A: contextWindowKnown must be false, got %v", m["contextWindowKnown"])
	}
	cw, ok := m["contextWindow"].(float64)
	if !ok || int64(cw) != runtimedetect.DefaultContextWindowFloor {
		t.Errorf("PR-A: contextWindow must be 200K floor, got %v", m["contextWindow"])
	}
}

func TestHandleAvailable_RejectsNonGet(t *testing.T) {
	s := &Server{RuntimeDetectCache: runtimedetect.NewMemoryCache()}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(m, "/api/v1/available", nil)
		rr := httptest.NewRecorder()
		s.handleAvailable(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", m, rr.Code)
		}
	}
}

// TestHandleAvailable_ReplicaConsistency: any control-plane replica
// reading from the SAME Cache instance (the production wiring is Redis →
// shared key) must return the SAME body byte-for-byte. This guards the AC
// "multiple replicas return the same /available response" — the test
// exercises the handler, not Redis, but the failure mode it catches is
// per-replica in-memory mutation (e.g., handler accidentally caching
// state on the Server struct).
func TestHandleAvailable_ReplicaConsistency(t *testing.T) {
	cache := runtimedetect.NewMemoryCache()
	if err := cache.Put(context.Background(), &runtimedetect.Snapshot{
		ClaudeCodeVersions: []string{"1.0.0"},
		Models:             []runtimedetect.Model{{ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5", ContextWindow: 200_000, ContextWindowKnown: false}},
		FetchedAt:          time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	// Replica A: fresh Server pointing at the shared cache.
	a := &Server{RuntimeDetectCache: cache}
	// Replica B: another fresh Server, same cache.
	b := &Server{RuntimeDetectCache: cache}

	bodyOf := func(s *Server) string {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/available", nil)
		rr := httptest.NewRecorder()
		s.handleAvailable(rr, req)
		return rr.Body.String()
	}
	if bodyOf(a) != bodyOf(b) {
		t.Fatalf("replicas diverged:\nA=%s\nB=%s", bodyOf(a), bodyOf(b))
	}
}
