package runtimedetect_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

// Setup helpers: spin up an npm fake and an Anthropic fake, return a
// poller wired to a memory cache.
func setupPoller(t *testing.T, npmHandler, anthropicHandler http.HandlerFunc, keyFn runtimedetect.KeySource) (*runtimedetect.Poller, *runtimedetect.MemoryCache, func()) {
	t.Helper()
	npmSrv := httptest.NewServer(npmHandler)
	anthropicSrv := httptest.NewServer(anthropicHandler)
	cache := runtimedetect.NewMemoryCache()
	p := &runtimedetect.Poller{
		Cache:        cache,
		Npm:          runtimedetect.NewNpmClient(npmSrv.URL, 2*time.Second),
		Anthropic:    runtimedetect.NewAnthropicClient(anthropicSrv.URL, "", 2*time.Second),
		KeySource:    keyFn,
		VersionLimit: 5,
	}
	return p, cache, func() {
		npmSrv.Close()
		anthropicSrv.Close()
	}
}

func TestPoller_PollOnce_PopulatesCache(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	npmBody := npmFixture(base, "1.0.0", "0.9.0", "1.0.0")
	p, cache, done := setupPoller(t,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(npmBody)) },
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(anthropicFixture)) },
		func() (string, error) { return "sk-test", nil },
	)
	defer done()

	p.PollOnce(context.Background())

	snap, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("cache empty after PollOnce: %v", err)
	}
	if len(snap.ClaudeCodeVersions) == 0 {
		t.Fatal("expected versions, got empty")
	}
	if snap.ClaudeCodeVersions[0] != "1.0.0" {
		t.Fatalf("expected latest at position 0, got %q", snap.ClaudeCodeVersions[0])
	}
	if len(snap.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(snap.Models))
	}
	if snap.FetchedAt.IsZero() {
		t.Fatal("expected FetchedAt populated")
	}
}

func TestPoller_PollOnce_MissingKey_KeepsCachedModels(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	npmBody := npmFixture(base, "1.0.0", "1.0.0")

	keyVal := atomic.Pointer[string]{}
	first := "sk-test"
	keyVal.Store(&first)
	keyFn := func() (string, error) {
		k := keyVal.Load()
		if k == nil {
			return "", nil
		}
		return *k, nil
	}

	p, cache, done := setupPoller(t,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(npmBody)) },
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(anthropicFixture)) },
		keyFn,
	)
	defer done()

	// First poll: key configured → models populated.
	p.PollOnce(context.Background())
	snap, err := cache.Get(context.Background())
	if err != nil || len(snap.Models) == 0 {
		t.Fatalf("first poll failed to populate models: err=%v snap=%+v", err, snap)
	}

	// Operator clears the key (rotation gap). Next poll: models should
	// remain the last-good list.
	keyVal.Store(nil)
	p.PollOnce(context.Background())
	snap2, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("cache emptied after key cleared: %v", err)
	}
	if len(snap2.Models) != len(snap.Models) {
		t.Fatalf("models list shrunk after key cleared: was %d, now %d", len(snap.Models), len(snap2.Models))
	}
}

func TestPoller_PollOnce_AnthropicFails_KeepsCachedModels(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	npmBody := npmFixture(base, "1.0.0", "1.0.0")

	anthropicCalls := atomic.Int32{}
	anthropicHandler := func(w http.ResponseWriter, r *http.Request) {
		n := anthropicCalls.Add(1)
		if n == 1 {
			_, _ = w.Write([]byte(anthropicFixture))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	p, cache, done := setupPoller(t,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(npmBody)) },
		anthropicHandler,
		func() (string, error) { return "sk-test", nil },
	)
	defer done()

	p.PollOnce(context.Background())
	snap, _ := cache.Get(context.Background())
	if len(snap.Models) == 0 {
		t.Fatal("first poll: expected models")
	}

	// Second poll: Anthropic 500s; cache must retain previous models.
	p.PollOnce(context.Background())
	snap2, _ := cache.Get(context.Background())
	if len(snap2.Models) != len(snap.Models) {
		t.Fatalf("anthropic failure dropped models: was %d, now %d", len(snap.Models), len(snap2.Models))
	}
}

func TestPoller_PollOnce_NpmFails_KeepsCachedVersions(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	npmBody := npmFixture(base, "1.0.0", "1.0.0")

	npmCalls := atomic.Int32{}
	npmHandler := func(w http.ResponseWriter, r *http.Request) {
		n := npmCalls.Add(1)
		if n == 1 {
			_, _ = w.Write([]byte(npmBody))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	p, cache, done := setupPoller(t,
		npmHandler,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(anthropicFixture)) },
		func() (string, error) { return "sk-test", nil },
	)
	defer done()

	p.PollOnce(context.Background())
	snap, _ := cache.Get(context.Background())
	if len(snap.ClaudeCodeVersions) == 0 {
		t.Fatal("first poll: expected versions")
	}

	p.PollOnce(context.Background())
	snap2, _ := cache.Get(context.Background())
	if len(snap2.ClaudeCodeVersions) != len(snap.ClaudeCodeVersions) {
		t.Fatalf("npm failure dropped versions: was %d, now %d", len(snap.ClaudeCodeVersions), len(snap2.ClaudeCodeVersions))
	}
}

func TestPoller_PollOnce_BothFailAndNoCache_LeavesCacheEmpty(t *testing.T) {
	p, cache, done := setupPoller(t,
		func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
		func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
		func() (string, error) { return "sk-test", nil },
	)
	defer done()

	p.PollOnce(context.Background())
	if _, err := cache.Get(context.Background()); !errors.Is(err, runtimedetect.ErrCacheEmpty) {
		t.Fatalf("expected ErrCacheEmpty after dual failure on empty cache, got %v", err)
	}
}

// fakeContextWindowResolver: an in-memory stand-in for
// pkg/contextwindowmap.Resolver. Used by the poller tests to assert the
// enrichment hook fires on every cycle (kyber#378 PR-D).
type fakeContextWindowResolver map[string]int64

func (f fakeContextWindowResolver) LookupOr(_ context.Context, id string) (int64, bool) {
	if v, ok := f[id]; ok {
		return v, true
	}
	return runtimedetect.DefaultContextWindowFloor, false
}

func TestPoller_PollOnce_EnrichesModelsWithContextWindowResolver(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	npmBody := npmFixture(base, "1.0.0", "1.0.0")

	p, cache, done := setupPoller(t,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(npmBody)) },
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(anthropicFixture)) },
		func() (string, error) { return "sk-test", nil },
	)
	defer done()

	// Override entry for opus only; sonnet should fall back to floor +
	// Known=false (preserving the AC's "context unknown" UX path).
	p.ContextWindows = fakeContextWindowResolver{"claude-opus-4-7": 1_000_000}

	p.PollOnce(context.Background())

	snap, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("cache empty: %v", err)
	}
	gotOpus, gotSonnet := false, false
	for _, m := range snap.Models {
		switch m.ID {
		case "claude-opus-4-7":
			gotOpus = true
			if m.ContextWindow != 1_000_000 || !m.ContextWindowKnown {
				t.Errorf("opus enrichment: got (%d, %v), want (1000000, true)", m.ContextWindow, m.ContextWindowKnown)
			}
		case "claude-sonnet-4-6":
			gotSonnet = true
			if m.ContextWindow != runtimedetect.DefaultContextWindowFloor || m.ContextWindowKnown {
				t.Errorf("sonnet fallback: got (%d, %v), want (%d, false)", m.ContextWindow, m.ContextWindowKnown, runtimedetect.DefaultContextWindowFloor)
			}
		}
	}
	if !gotOpus || !gotSonnet {
		t.Fatalf("missing expected models in snapshot: opus=%v sonnet=%v", gotOpus, gotSonnet)
	}
}

// kyber#488: the enrichment loop must NOT clobber an auto-detected window.
// Before the override-only fix, the loop called LookupOr unconditionally and
// overwrote every model with (floor, false) for any ID absent from the
// ConfigMap — erasing the window the poller just decoded from
// max_input_tokens. This guards that regression and confirms the ConfigMap
// still wins when it carries an entry.
func TestPoller_PollOnce_ConfigMapOverridesButDoesNotClobberDetected(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	npmBody := npmFixture(base, "1.0.0", "1.0.0")

	p, cache, done := setupPoller(t,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(npmBody)) },
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(anthropicFixtureWithWindows)) },
		func() (string, error) { return "sk-test", nil },
	)
	defer done()

	// claude-opus-4-8 is detected at 1M (max_input_tokens) but is ABSENT from
	// the override map. claude-legacy-1 omits max_input_tokens (detected as
	// floor+false) but the operator pins it to 500K via the ConfigMap.
	p.ContextWindows = fakeContextWindowResolver{"claude-legacy-1": 500_000}

	p.PollOnce(context.Background())

	snap, err := cache.Get(context.Background())
	if err != nil {
		t.Fatalf("cache empty: %v", err)
	}
	gotOpus, gotLegacy := false, false
	for _, m := range snap.Models {
		switch m.ID {
		case "claude-opus-4-8":
			gotOpus = true
			// Absent from ConfigMap → detected window survives (not clobbered).
			if m.ContextWindow != 1_000_000 || !m.ContextWindowKnown {
				t.Errorf("opus (detected, no override): got (%d, %v), want (1000000, true)", m.ContextWindow, m.ContextWindowKnown)
			}
		case "claude-legacy-1":
			gotLegacy = true
			// Present in ConfigMap → override wins over the floor default.
			if m.ContextWindow != 500_000 || !m.ContextWindowKnown {
				t.Errorf("legacy (override present): got (%d, %v), want (500000, true)", m.ContextWindow, m.ContextWindowKnown)
			}
		}
	}
	if !gotOpus || !gotLegacy {
		t.Fatalf("missing expected models: opus=%v legacy=%v", gotOpus, gotLegacy)
	}
}

func TestPoller_Start_StopsOnContextCancel(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	npmBody := npmFixture(base, "1.0.0", "1.0.0")
	p, _, done := setupPoller(t,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(npmBody)) },
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(anthropicFixture)) },
		func() (string, error) { return "sk-test", nil },
	)
	defer done()
	p.Cadence = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- p.Start(ctx) }()

	// Let it run a couple of ticks so the loop is actually inside select.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Start returned error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return within 2s after context cancel")
	}
}
