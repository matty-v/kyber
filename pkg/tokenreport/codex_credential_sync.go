package tokenreport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
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

	// Seed lastHash from what is already on disk so the first tick does not
	// re-push the credential the pod just booted with. A push there would be
	// harmless but pointless write traffic against the Secret on every boot.
	lastHash := ""
	if data, err := s.read(); err == nil && !s.PushInitial {
		lastHash = hashCredential(data)
	}

	backoff := time.Second
	maxBackoff := 5 * time.Minute

	interval := s.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	doTick := func() {
		newHash, err := s.tick(ctx, client, lastHash)
		if err != nil {
			log.Printf("[codex-credential-sync] error: %v (backing off %s)", err, backoff)
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
		if newHash != "" {
			lastHash = newHash
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
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// tick reads auth.json and pushes it when its content hash has changed.
// Returns the new hash on a successful push, "" when no push was needed,
// or an error to trigger backoff.
func (s *CodexCredentialSyncer) tick(ctx context.Context, client *http.Client, lastHash string) (string, error) {
	data, err := s.read()
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // pre-login or api-key agent — skip silently
		}
		return "", fmt.Errorf("read auth.json: %w", err)
	}

	// A partially-written file must never overwrite a good Secret. Codex
	// writes atomically, but the poll path can still catch a torn file on a
	// filesystem that does not honour rename atomicity.
	if !json.Valid(data) {
		log.Printf("[codex-credential-sync] skipping malformed auth.json (%d bytes)", len(data))
		return "", nil
	}

	hash := hashCredential(data)
	if hash == lastHash {
		return "", nil // unchanged
	}

	body, err := json.Marshal(map[string]any{"auth_json": string(data)})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.rotationURL(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("post codex-auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("codex-auth endpoint returned %d", resp.StatusCode)
	}

	// Log the hash prefix only — enough to correlate a push with a Secret
	// generation while never revealing the credential.
	log.Printf("[codex-credential-sync] pushed refreshed credentials (%s… → %s…)",
		firstN(lastHash, 8), firstN(hash, 8))
	return hash, nil
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
