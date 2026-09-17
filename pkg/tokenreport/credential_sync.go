package tokenreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/matty-v/kyber/pkg/credentialsync"
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

	// InitialCredentialHash identifies the complete credential in the Secret
	// that boot started from and is sent as the write-back precondition.
	// Startup sets PushInitial when a known pending local rotation must be
	// reconciled immediately.
	InitialCredentialHash string
	PushInitial           bool

	// Configurable for deterministic tests. Zero values use 1s -> 5m.
	RetryInitial time.Duration
	RetryMax     time.Duration

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

	expectedHash := s.InitialCredentialHash
	lastLocalHash := ""
	localHash := ""
	if creds, err := s.read(); err == nil {
		localHash = hashClaudeCredential(creds.ClaudeAiOauth)
		if !s.PushInitial {
			lastLocalHash = localHash
		}
	}

	retryInitial := s.RetryInitial
	if retryInitial <= 0 {
		retryInitial = time.Second
	}
	retryMax := s.RetryMax
	if retryMax <= 0 {
		retryMax = 5 * time.Minute
	}
	backoff := retryInitial

	interval := s.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	var retryTimer *time.Timer
	var retryC <-chan time.Time
	stopRetry := func() {
		if retryTimer != nil && !retryTimer.Stop() {
			select {
			case <-retryTimer.C:
			default:
			}
		}
		retryC = nil
	}
	scheduleRetry := func() {
		stopRetry()
		retryTimer = time.NewTimer(backoff)
		retryC = retryTimer.C
		backoff *= 2
		if backoff > retryMax {
			backoff = retryMax
		}
	}
	defer stopRetry()

	doTick := func() {
		observedHash, pushed, err := s.tick(ctx, client, lastLocalHash, expectedHash)
		if err != nil {
			if errors.Is(err, ErrCredentialSuperseded) {
				lastLocalHash = observedHash
				stopRetry()
				backoff = retryInitial
				log.Printf("[credential-sync] local rotation superseded by newer Secret; restart required")
				return
			}
			if errors.Is(err, ErrCredentialRejected) {
				lastLocalHash = observedHash
				stopRetry()
				backoff = retryInitial
				log.Printf("[credential-sync] local credential was rejected; fix runtime authorization or credential format before retrying: %v", err)
				return
			}
			log.Printf("[credential-sync] error: %v (backing off %s)", err, backoff)
			scheduleRetry()
			return
		}
		stopRetry()
		backoff = retryInitial
		if pushed {
			lastLocalHash = observedHash
			expectedHash = observedHash
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
	if s.PushInitial {
		doTick()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if retryC == nil {
				doTick()
			}
		case <-retryC:
			retryC = nil
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
			stopRetry()
			doTick()
		}
	}
}

// ErrCredentialSuperseded means the Secret changed after this runtime read its
// bootstrap credential. Retrying would risk overwriting operator reauthorization.
var ErrCredentialSuperseded = errors.New("credential write superseded")

// ErrCredentialRejected means the control plane permanently rejected this
// snapshot (for example, malformed input or failed runtime authorization).
// Retrying unchanged content cannot succeed and would obscure the diagnosis.
var ErrCredentialRejected = errors.New("credential write rejected")

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

func (s *CredentialSyncer) read() (credentialFile, error) {
	data, err := os.ReadFile(s.CredentialsPath)
	if err != nil {
		return credentialFile{}, err
	}
	var creds credentialFile
	if err := json.Unmarshal(data, &creds); err != nil {
		return credentialFile{}, fmt.Errorf("%w: %v", errMalformedClaudeCredential, err)
	}
	return creds, nil
}

func hashClaudeCredential(oauth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}) string {
	return credentialsync.HashClaude(oauth.AccessToken, oauth.RefreshToken, oauth.ExpiresAt)
}

// tick reads the credentials file and pushes it when the complete credential
// hash changed. observedHash identifies the attempted snapshot even on error;
// pushed means the Secret confirmed or already contained that snapshot.
func (s *CredentialSyncer) tick(ctx context.Context, client *http.Client, lastHash, expectedHash string) (observedHash string, pushed bool, err error) {
	creds, err := s.read()
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil // api-key agent or pre-login — skip silently
		}
		// The file may be mid-write. JSON syntax errors are not retried until
		// another fsnotify/poll event; a corrupt snapshot must never reach the
		// Secret.
		if errors.Is(err, errMalformedClaudeCredential) {
			log.Printf("[credential-sync] skipping malformed credentials file")
			return "", false, nil
		}
		return "", false, fmt.Errorf("read credentials: %w", err)
	}

	oauth := creds.ClaudeAiOauth
	if oauth.AccessToken == "" || oauth.RefreshToken == "" || oauth.ExpiresAt == 0 {
		return "", false, nil // incomplete credentials — nothing to sync
	}

	hash := hashClaudeCredential(oauth)
	if hash == lastHash {
		return "", false, nil // unchanged — no push needed
	}

	// Credentials changed — push to rotation endpoint
	body, err := json.Marshal(struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
		ExpectedHash string `json:"expected_hash,omitempty"`
	}{
		AccessToken: oauth.AccessToken, RefreshToken: oauth.RefreshToken,
		ExpiresAt: oauth.ExpiresAt, ExpectedHash: expectedHash,
	})
	if err != nil {
		return hash, false, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.rotationURL(), bytes.NewReader(body))
	if err != nil {
		return hash, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return hash, false, fmt.Errorf("post rotation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return hash, false, ErrCredentialSuperseded
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return hash, false, fmt.Errorf("%w: rotation endpoint returned %d", ErrCredentialRejected, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return hash, false, fmt.Errorf("rotation endpoint returned %d", resp.StatusCode)
	}

	log.Printf("[credential-sync] pushed updated credentials (%s…)", firstN(hash, 8))
	return hash, true, nil
}

var errMalformedClaudeCredential = errors.New("malformed Claude credential")
