package tokenreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Reporter periodically parses the Claude Code JSONL and POSTs the
// resulting Snapshot to the kyber-status-sidecar's localhost forwarder
// (kyber#257). The sidecar forwards to the control plane's
// /internal/agents/{name}/token-usage endpoint with auth applied.
//
// Pre-#257 the reporter POSTed directly to the control plane via
// ControlPlaneURL+PodToken; that's gone, the sidecar is now the sole
// in-pod-to-control-plane conduit.
//
// AgentName is kept on the struct only because the activity-detector
// shares this struct's caller surface; the value isn't sent on the wire
// anymore (the sidecar adds it from its own AGENT_NAME env).
type Reporter struct {
	AgentName   string
	ProjectsDir string // absolute path to the session-files dir
	// SidecarURL is the base URL of the in-pod sidecar's localhost
	// forwarder, e.g. "http://127.0.0.1:8091". Defaults to the standard
	// localhost address when zero.
	SidecarURL string
	Interval   time.Duration

	// HTTPClient is optional; defaults to http.Client with a 5s timeout.
	HTTPClient *http.Client
}

// reporterURL is the resolved POST target — either the configured override
// or the localhost forwarder default. Pulled out so tests can drive the
// fallback path explicitly.
func (r *Reporter) reporterURL() string {
	base := r.SidecarURL
	if base == "" {
		base = "http://127.0.0.1:8091"
	}
	return base + "/token-usage"
}

// Run blocks until ctx is cancelled, polling and posting on Interval.
// Errors are logged but never propagated — the reporter must never crash
// the agent pod.
func (r *Reporter) Run(ctx context.Context) {
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	backoff := time.Second
	maxBackoff := 5 * time.Minute

	tick := time.NewTicker(r.Interval)
	defer tick.Stop()

	// Fire once immediately so the first data point shows up quickly.
	_ = r.tick(ctx, client)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := r.tick(ctx, client); err != nil {
				log.Printf("[token-reporter] tick error: %v (backing off %s)", err, backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			} else {
				backoff = time.Second // reset on success
			}
		}
	}
}

// tick does one parse + post. Returns nil on success (including "no snapshot
// available yet") or an error to trigger backoff.
func (r *Reporter) tick(ctx context.Context, client *http.Client) error {
	// Find the newest .jsonl in ProjectsDir. If the dir is empty, treat as
	// "no data yet" — not an error.
	latest, err := FindLatestSessionFile(r.ProjectsDir)
	if err != nil {
		return nil
	}
	// ParseLatest takes dir + file separately; split latest.
	snap, err := ParseLatest(dirOf(latest), baseOf(latest))
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if snap == nil {
		return nil // no finalized message yet
	}
	body, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.reporterURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("post: status=%d", resp.StatusCode)
	}
	return nil
}

// Small helpers — avoid dragging filepath into the header imports if we only
// use them once.
func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}

func baseOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
