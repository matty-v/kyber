package tokenreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// CredentialSyncer watches ~/.claude/.credentials.json and pushes changed
// credentials to the control plane's rotation endpoint. This ensures that
// runtime token refreshes performed by Claude Code's internal keychain are
// persisted to the k8s Secret, preventing stale-token NeedsAuth on restart.
//
// Two trigger modes operate together:
//   - **fsnotify** on the parent directory catches atomic-rename writes
//     (claude-code's typical pattern: write-to-tmp + rename) within ms,
//     so a refresh→pod-death sequence loses at most a few hundred ms of
//     state, not up to one polling Interval. This is the primary defense
//     against the cold-boot stale-token race documented in kyber#273.
//   - **Interval polling** is the fallback for environments where
//     fsnotify can't be set up (rare; container fs always supports it on
//     k3s/Linux), and as a heartbeat in case an fsnotify event is missed.
//     Set this conservatively (the previous default of 1h was too long
//     to be useful as the *primary* signal but is fine as a backstop;
//     5m is a reasonable middle ground when fsnotify is also active).
type CredentialSyncer struct {
	CredentialsPath string // absolute path to .credentials.json
	// SidecarURL is the base URL of the in-pod sidecar's localhost
	// forwarder, e.g. "http://127.0.0.1:8091". The syncer POSTs to
	// {SidecarURL}/refresh-token; the sidecar applies the per-agent URL
	// prefix and pod-token auth before forwarding to the control plane
	// (kyber#257). Defaults to the standard localhost address when zero.
	SidecarURL string
	Interval   time.Duration

	// InitialExpiresAt seeds the last-pushed state from boot-time env vars.
	// If non-zero, the first tick skips pushing unless expiresAt has changed.
	InitialExpiresAt int64

	HTTPClient *http.Client
}

// rotationURL is the resolved POST target.
func (s *CredentialSyncer) rotationURL() string {
	base := s.SidecarURL
	if base == "" {
		base = "http://127.0.0.1:8091"
	}
	return base + "/refresh-token"
}

// credentialFile mirrors the structure of ~/.claude/.credentials.json.
type credentialFile struct {
	ClaudeAiOauth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

// Run blocks until ctx is cancelled. Pushes credentials to the rotation
// endpoint when either (a) fsnotify reports a Write/Create on the
// credentials file, or (b) the polling Interval fires. Errors are logged
// but never propagated — the syncer must never crash the pod.
func (s *CredentialSyncer) Run(ctx context.Context) {
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	lastExpiresAt := s.InitialExpiresAt
	backoff := time.Second
	maxBackoff := 5 * time.Minute

	tick := time.NewTicker(s.Interval)
	defer tick.Stop()

	doTick := func() {
		newExpiresAt, err := s.tick(ctx, client, lastExpiresAt)
		if err != nil {
			log.Printf("[credential-sync] error: %v (backing off %s)", err, backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			return
		}
		backoff = time.Second
		if newExpiresAt != 0 {
			lastExpiresAt = newExpiresAt
		}
	}

	// fsTrigger is signalled when fsnotify sees a Write/Create on the
	// credentials file. Buffered so the watcher goroutine never blocks;
	// we coalesce bursts (atomic write-then-rename) by draining and
	// debouncing in the main loop.
	fsTrigger := make(chan struct{}, 1)
	nudge := func() {
		select {
		case fsTrigger <- struct{}{}:
		default:
		}
	}

	if watcher, ok := s.startFSWatcher(ctx, nudge); ok {
		defer watcher.Close()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			doTick()
		case <-fsTrigger:
			// Debounce: claude-code's atomic-rename emits Create+Write in
			// quick succession, so wait briefly and drain any extra
			// events that arrive while settling — one push per refresh.
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			select {
			case <-fsTrigger:
			default:
			}
			doTick()
		}
	}
}

// startFSWatcher arms an fsnotify watch on the parent directory of
// CredentialsPath. See watchCredentialFile for the mechanics.
func (s *CredentialSyncer) startFSWatcher(ctx context.Context, nudge func()) (*fsnotify.Watcher, bool) {
	return watchCredentialFile(ctx, s.CredentialsPath, nudge, "credential-sync")
}

// watchCredentialFile arms an fsnotify watch on the parent directory of path.
// Returns (watcher, true) on success, or (nil, false) when fsnotify can't be
// set up — caller falls back to polling-only. nudge() is invoked whenever the
// watcher sees a Write/Create event whose target is exactly path.
//
// Watching the directory (not the file) is required because both harnesses
// rewrite credentials via atomic rename: an inode-pinned watch on the file
// misses the rename's CREATE-on-target.
//
// Shared by the Claude Code syncer (.credentials.json) and the Codex syncer
// (auth.json) — the watch mechanics are identical; only the payload differs.
func watchCredentialFile(ctx context.Context, path string, nudge func(), logPrefix string) (*fsnotify.Watcher, bool) {
	parent := filepath.Dir(path)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[%s] fsnotify init failed: %v — polling-only fallback", logPrefix, err)
		return nil, false
	}
	if err := watcher.Add(parent); err != nil {
		log.Printf("[%s] fsnotify watch %s failed: %v — polling-only fallback", logPrefix, parent, err)
		_ = watcher.Close()
		return nil, false
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name != path {
					continue
				}
				// Both Create (atomic rename) and Write (in-place edit)
				// can deliver fresh credentials. Remove/Rename without
				// a follow-up Create on this name means the file went
				// away — nothing to push.
				if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
					nudge()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("[%s] fsnotify error: %v", logPrefix, err)
			}
		}
	}()

	log.Printf("[%s] fsnotify watching %s", logPrefix, parent)
	return watcher, true
}

// tick reads the credentials file and pushes to the rotation endpoint if
// expiresAt has changed. Returns the new expiresAt on successful push,
// 0 if no push was needed, or an error to trigger backoff.
func (s *CredentialSyncer) tick(ctx context.Context, client *http.Client, lastExpiresAt int64) (int64, error) {
	data, err := os.ReadFile(s.CredentialsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // api-key agent or pre-login — skip silently
		}
		return 0, fmt.Errorf("read credentials: %w", err)
	}

	var creds credentialFile
	if err := json.Unmarshal(data, &creds); err != nil {
		// File may be mid-write — skip this tick
		log.Printf("[credential-sync] skipping malformed credentials file: %v", err)
		return 0, nil
	}

	oauth := creds.ClaudeAiOauth
	if oauth.AccessToken == "" || oauth.RefreshToken == "" || oauth.ExpiresAt == 0 {
		return 0, nil // incomplete credentials — nothing to sync
	}

	if oauth.ExpiresAt == lastExpiresAt {
		return 0, nil // unchanged — no push needed
	}

	// Credentials changed — push to rotation endpoint
	body, err := json.Marshal(map[string]any{
		"access_token":  oauth.AccessToken,
		"refresh_token": oauth.RefreshToken,
		"expires_at":    oauth.ExpiresAt,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.rotationURL(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post rotation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("rotation endpoint returned %d", resp.StatusCode)
	}

	log.Printf("[credential-sync] pushed updated credentials (expiresAt: %d → %d)", lastExpiresAt, oauth.ExpiresAt)
	return oauth.ExpiresAt, nil
}
