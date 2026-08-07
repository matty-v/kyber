package tokenreport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func writeCredentials(t *testing.T, path string, access, refresh string, expiresAt int64) {
	t.Helper()
	data, err := json.Marshal(credentialFile{
		ClaudeAiOauth: struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		}{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresAt:    expiresAt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialSyncer_PushesOnChange(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")

	var pushCount atomic.Int32
	var lastBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		// kyber#257: the sidecar adds auth on the outbound hop, so the
		// in-pod syncer doesn't set Authorization on its localhost POST.
		if got := r.URL.Path; got != "/refresh-token" {
			t.Errorf("expected POST /refresh-token (sidecar localhost path), got %s", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		lastBody = body
		pushCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Write initial credentials with expiresAt=1000
	writeCredentials(t, credsPath, "at-initial", "rt-initial", 1000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	syncer := &CredentialSyncer{
		CredentialsPath:  credsPath,
		SidecarURL:       srv.URL,
		Interval:         50 * time.Millisecond,
		InitialExpiresAt: 1000, // matches file — should NOT push
	}

	go syncer.Run(ctx)

	// Wait a couple ticks — should not push (expiresAt unchanged)
	time.Sleep(150 * time.Millisecond)
	if pushCount.Load() != 0 {
		t.Fatalf("expected 0 pushes for unchanged credentials, got %d", pushCount.Load())
	}

	// Simulate Claude Code refresh — write new credentials
	writeCredentials(t, credsPath, "at-refreshed", "rt-refreshed", 2000)

	// Wait for syncer to pick it up
	deadline := time.After(2 * time.Second)
	for pushCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for credential push")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	if pushCount.Load() != 1 {
		t.Fatalf("expected exactly 1 push, got %d", pushCount.Load())
	}
	if lastBody["access_token"] != "at-refreshed" {
		t.Errorf("expected access_token=at-refreshed, got %v", lastBody["access_token"])
	}
	if lastBody["refresh_token"] != "rt-refreshed" {
		t.Errorf("expected refresh_token=rt-refreshed, got %v", lastBody["refresh_token"])
	}
	if lastBody["expires_at"] != float64(2000) {
		t.Errorf("expected expires_at=2000, got %v", lastBody["expires_at"])
	}
}

func TestCredentialSyncer_SkipsWhenFileIsMissing(t *testing.T) {
	var pushCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	syncer := &CredentialSyncer{
		CredentialsPath: filepath.Join(t.TempDir(), "nonexistent.json"),
		SidecarURL:      srv.URL,
		Interval:        50 * time.Millisecond,
	}

	go syncer.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	if pushCount.Load() != 0 {
		t.Fatalf("expected 0 pushes for missing file, got %d", pushCount.Load())
	}
}

func TestCredentialSyncer_SkipsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")

	// Write garbage
	if err := os.WriteFile(credsPath, []byte("{invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var pushCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	syncer := &CredentialSyncer{
		CredentialsPath: credsPath,
		SidecarURL:      srv.URL,
		Interval:        50 * time.Millisecond,
	}

	go syncer.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	if pushCount.Load() != 0 {
		t.Fatalf("expected 0 pushes for malformed JSON, got %d", pushCount.Load())
	}
}

// TestCredentialSyncer_FSNotifyTriggersBeforePoll pins kyber#273.
// With Interval set to a long duration, the syncer must still push
// within ~1s of a credentials write — proving fsnotify is the primary
// trigger, not polling. Pre-#273 this took up to 1h (the previous poll
// interval), which left a wide cold-boot stale-token window.
func TestCredentialSyncer_FSNotifyTriggersBeforePoll(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")

	var pushCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	writeCredentials(t, credsPath, "at-initial", "rt-initial", 1000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Interval is long (1 minute) — if the test passes within ~1s, it must
	// be fsnotify, not polling. InitialExpiresAt=1000 matches file so
	// startup tick is suppressed.
	syncer := &CredentialSyncer{
		CredentialsPath:  credsPath,
		SidecarURL:       srv.URL,
		Interval:         60 * time.Second,
		InitialExpiresAt: 1000,
	}
	go syncer.Run(ctx)

	// Give fsnotify a moment to attach the watch (NewWatcher → Add).
	time.Sleep(100 * time.Millisecond)

	// Trigger a credential rotation. Push should land within ~1s
	// (fsnotify event + 200ms debounce + http roundtrip), well below
	// the 60s polling interval.
	writeCredentials(t, credsPath, "at-refreshed", "rt-refreshed", 2000)

	deadline := time.After(2 * time.Second)
	for pushCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("fsnotify did not deliver push within 2s — kyber#273 regression?")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	if pushCount.Load() != 1 {
		t.Fatalf("expected 1 push from fsnotify trigger, got %d", pushCount.Load())
	}
}

// TestCredentialSyncer_AtomicRenamePushesOnce verifies the production
// write pattern: write to temp file → rename to target. fsnotify
// emits Create+Write on the temp followed by Create on the target;
// the syncer's filename filter + 200ms debounce must coalesce that
// into exactly one push for one rotation. (Without debounce, claude
// code's atomic-rename could emit duplicate pushes; without the
// filename filter, we'd push on every temp-file write too.)
func TestCredentialSyncer_AtomicRenamePushesOnce(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")

	var pushCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Initial state matches the seed so startup doesn't fire.
	writeCredentials(t, credsPath, "at-initial", "rt-initial", 1000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	syncer := &CredentialSyncer{
		CredentialsPath:  credsPath,
		SidecarURL:       srv.URL,
		Interval:         60 * time.Second,
		InitialExpiresAt: 1000,
	}
	go syncer.Run(ctx)

	time.Sleep(100 * time.Millisecond)

	// Atomic rename pattern: write to .tmp first, then rename onto target.
	tmpPath := credsPath + ".tmp"
	data, _ := json.Marshal(credentialFile{
		ClaudeAiOauth: struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		}{AccessToken: "at-rotated", RefreshToken: "rt-rotated", ExpiresAt: 3000}},
	)
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, credsPath); err != nil {
		t.Fatal(err)
	}

	// Wait long enough for any debounced events to settle (200ms debounce
	// + roundtrip + a comfortable margin).
	deadline := time.After(2 * time.Second)
	for pushCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no push observed after atomic rename — fsnotify path broken")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Hold for additional time to ensure a duplicate doesn't sneak in.
	time.Sleep(500 * time.Millisecond)
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 push from atomic rename, got %d", got)
	}
}

func TestCredentialSyncer_NoPushWithoutInitialSeed(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, ".credentials.json")

	writeCredentials(t, credsPath, "at-1", "rt-1", 5000)

	var pushCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// InitialExpiresAt=0 means "no boot-time seed" — first tick should push
	// because 5000 != 0
	syncer := &CredentialSyncer{
		CredentialsPath:  credsPath,
		SidecarURL:       srv.URL,
		Interval:         50 * time.Millisecond,
		InitialExpiresAt: 0,
	}

	go syncer.Run(ctx)

	deadline := time.After(2 * time.Second)
	for pushCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for first push")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	if pushCount.Load() != 1 {
		t.Fatalf("expected 1 push, got %d", pushCount.Load())
	}
}
