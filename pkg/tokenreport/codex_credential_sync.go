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
	"time"

	"github.com/matty-v/kyber/pkg/credentialsync"
)

// CodexCredentialSyncer watches ~/.codex/auth.json and pushes the document to
// the control plane whenever it changes, so that CLI-performed token refreshes
// are persisted back into the agent's <name>-codex-auth Secret.
//
// This is the Codex counterpart of CredentialSyncer, and it exists for a
// sharper reason than its Claude Code sibling (kyber#681).
//
// ChatGPT refresh tokens are SINGLE USE. Every refresh burns the old token and
// returns a new one. Without write-back, the Secret keeps a refresh token that
// the first post-boot refresh already consumed, so the credential is not merely
// stale — it is permanently dead, and every later boot restores that dead copy.
// HK-47 died exactly this way on 2026-08-04: `your refresh token was already
// used. Please log out and sign in again`, ten days after his login.
//
// Unlike the Claude Code syncer, this one treats auth.json as OPAQUE. Codex owns
// that document's shape; parsing it here would couple Kyber to an upstream
// format we do not control and would risk dropping fields on the round trip. We
// therefore forward the bytes verbatim and dedupe on a content hash rather than
// on an expiry field. The hash is of the credential, so it is never logged.
type CodexCredentialSyncer struct {
	// AuthPath is the absolute path to auth.json.
	AuthPath string
	// SidecarURL is the base URL of the in-pod sidecar's localhost
	// forwarder, e.g. "http://127.0.0.1:8091". The syncer POSTs to
	// {SidecarURL}/codex-auth; the sidecar applies the per-agent URL prefix
	// and pod-token auth before forwarding. Defaults to the standard
	// localhost address when zero.
	SidecarURL string
	// Interval is the fsnotify-miss backstop poll period.
	Interval time.Duration
	// PushInitial forces the credential already present when Run starts to be
	// written once. Device-auth boots use this because the Secret contained only
	// Kyber's {} marker, not the credential the CLI just created.
	PushInitial bool
	// InitialCredentialHash identifies the opaque auth.json stored in the
	// bootstrap Secret and is sent as the write-back precondition.
	InitialCredentialHash string

	// Configurable for deterministic tests. Zero values use 1s -> 5m.
	RetryInitial time.Duration
	RetryMax     time.Duration

	HTTPClient *http.Client
}

// maxCodexAuthBytes bounds what we are willing to read and forward. The API
// applies the same 256KiB ceiling when accepting codexAuthJson at create time
// (routes_agents.go), so a document larger than this could never have been
// stored in the Secret and indicates a corrupt or wrong file.
const maxCodexAuthBytes = 256 << 10

func (s *CodexCredentialSyncer) rotationURL() string {
	base := s.SidecarURL
	if base == "" {
		base = "http://127.0.0.1:8091"
	}
	return base + "/codex-auth"
}

// Run blocks until ctx is cancelled, pushing auth.json whenever it changes.
// Errors are logged but never propagated — the syncer must never crash the pod.
func (s *CodexCredentialSyncer) Run(ctx context.Context) {
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	expectedHash := s.InitialCredentialHash
	lastHash := ""
	localHash := ""
	if data, err := s.read(); err == nil {
		localHash = hashCredential(data)
		if !s.PushInitial {
			lastHash = localHash
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
		newHash, pushed, err := s.tick(ctx, client, lastHash, expectedHash)
		if err != nil {
			if errors.Is(err, ErrCredentialSuperseded) {
				lastHash = newHash
				stopRetry()
				backoff = retryInitial
				log.Printf("[codex-credential-sync] local rotation superseded by newer Secret; restart required")
				return
			}
			if errors.Is(err, ErrCredentialRejected) {
				lastHash = newHash
				stopRetry()
				backoff = retryInitial
				log.Printf("[codex-credential-sync] local credential was rejected; fix runtime authorization or credential format before retrying: %v", err)
				return
			}
			log.Printf("[codex-credential-sync] error: %v (backing off %s)", err, backoff)
			scheduleRetry()
			return
		}
		stopRetry()
		backoff = retryInitial
		if pushed {
			lastHash = newHash
			expectedHash = newHash
		}
	}

	fsTrigger := make(chan struct{}, 1)
	nudge := func() {
		select {
		case fsTrigger <- struct{}{}:
		default:
		}
	}

	if watcher, ok := watchCredentialFile(ctx, s.AuthPath, nudge, "codex-credential-sync"); ok {
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
			// Debounce: an atomic write-then-rename emits Create+Write in
			// quick succession, so wait briefly and drain extras that
			// arrive while settling — one push per refresh.
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

// read loads auth.json, enforcing the size ceiling.
func (s *CodexCredentialSyncer) read() ([]byte, error) {
	f, err := os.Open(s.AuthPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, maxCodexAuthBytes+1)
	n, err := f.Read(data)
	if err != nil && n == 0 {
		return nil, err
	}
	if n > maxCodexAuthBytes {
		return nil, fmt.Errorf("auth.json exceeds %d bytes", maxCodexAuthBytes)
	}
	return data[:n], nil
}

func hashCredential(data []byte) string {
	return credentialsync.HashOpaque(data)
}

// tick reads auth.json and pushes it when its content hash has changed.
// Returns the new hash on a successful push, "" when no push was needed,
// or an error to trigger backoff.
func (s *CodexCredentialSyncer) tick(ctx context.Context, client *http.Client, lastHash, expectedHash string) (observedHash string, pushed bool, err error) {
	data, err := s.read()
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil // pre-login or api-key agent — skip silently
		}
		return "", false, fmt.Errorf("read auth.json: %w", err)
	}

	// A partially-written file must never overwrite a good Secret. Codex
	// writes atomically, but the poll path can still catch a torn file on a
	// filesystem that does not honour rename atomicity.
	if !json.Valid(data) {
		log.Printf("[codex-credential-sync] skipping malformed auth.json (%d bytes)", len(data))
		return "", false, nil
	}

	hash := hashCredential(data)
	if hash == lastHash {
		return "", false, nil // unchanged
	}

	body, err := json.Marshal(struct {
		AuthJSON     string `json:"auth_json"`
		ExpectedHash string `json:"expected_hash,omitempty"`
	}{AuthJSON: string(data), ExpectedHash: expectedHash})
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
		return hash, false, fmt.Errorf("post codex-auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return hash, false, ErrCredentialSuperseded
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return hash, false, fmt.Errorf("%w: codex-auth endpoint returned %d", ErrCredentialRejected, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return hash, false, fmt.Errorf("codex-auth endpoint returned %d", resp.StatusCode)
	}

	// Log the hash prefix only — enough to correlate a push with a Secret
	// generation while never revealing the credential.
	log.Printf("[codex-credential-sync] pushed refreshed credentials (%s… → %s…)",
		firstN(lastHash, 8), firstN(hash, 8))
	return hash, true, nil
}

func firstN(s string, n int) string {
	if s == "" {
		return "none"
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}
