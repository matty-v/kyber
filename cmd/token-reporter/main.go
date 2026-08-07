// Package main is the entrypoint for the Kyber token-usage reporter.
//
// Runs inside every claude-code agent pod. Polls the Claude Code JSONL
// transcript every KYBER_TOKEN_REPORT_INTERVAL (default 30s) and POSTs a
// Snapshot via the in-pod kyber-status-sidecar's localhost forwarder
// (kyber#257). The sidecar handles auth + onward delivery to the control
// plane; this binary doesn't need to know any control-plane URL or token.
//
// Required env:
//
//	AGENT_NAME — set by controller
//	HOME       — standard; the JSONL dir is $HOME/.claude/projects
//
// Optional env:
//
//	KYBER_TOKEN_REPORT_INTERVAL — default "30s" (Go duration)
//	KYBER_SIDECAR_URL           — default "http://127.0.0.1:8091" (test override)
//
// The reporter never exits on error; it logs and retries. SIGTERM shuts it down.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

func main() {
	agent := os.Getenv("AGENT_NAME")
	home := os.Getenv("HOME")
	if agent == "" || home == "" {
		log.Fatalf("token-reporter: AGENT_NAME and HOME must be set")
	}

	interval := 30 * time.Second
	if s := os.Getenv("KYBER_TOKEN_REPORT_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			log.Fatalf("token-reporter: invalid KYBER_TOKEN_REPORT_INTERVAL %q: %v", s, err)
		}
		interval = d
	}

	// Sidecar localhost forwarder (kyber#257). Empty falls through to the
	// in-pkg default; tests / dev installs can override.
	sidecarURL := os.Getenv("KYBER_SIDECAR_URL")

	projectsRoot := filepath.Join(home, ".claude", "projects")

	// ---- Credential syncer ----
	// Watches ~/.claude/.credentials.json and pushes runtime token refreshes
	// through the sidecar's /refresh-token path. Prevents stale-token
	// NeedsAuth after long-running sessions.
	var initialExpiresAt int64
	if s := os.Getenv("CLAUDE_ACCESS_TOKEN_EXPIRES_AT"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			initialExpiresAt = v
		}
	}

	credsPath := filepath.Join(home, ".claude", ".credentials.json")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Credential sync is unconditional now — the sidecar is always present
	// in agent pods (the controller injects it; api-key agents simply have
	// no .credentials.json on disk and the syncer's tick treats missing-
	// file as a no-op).
	syncer := &tokenreport.CredentialSyncer{
		CredentialsPath: credsPath,
		SidecarURL:      sidecarURL,
		// Polling Interval is the fsnotify-miss backstop; primary
		// trigger is fsnotify on the credentials directory (kyber#273).
		// 5m is short enough that a missed event loses at most one
		// poll window, vs. the prior 1h cold-boot stale-token window
		// that bit chewie 2026-05-06.
		Interval:         5 * time.Minute,
		InitialExpiresAt: initialExpiresAt,
	}
	go syncer.Run(ctx)
	log.Printf("token-reporter: credential syncer started (fsnotify primary + 5m poll backstop, via sidecar at %s)",
		nonEmptyOr(sidecarURL, "127.0.0.1:8091"))

	// Outer loop: every 60s re-pick the newest subdir under projectsRoot. If
	// it rotates (user ran /clear), restart the inner Reporter with the new
	// dir. The Reporter itself reads the newest .jsonl inside the provided
	// dir on every tick, so this is only needed for subdir changes.
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	currentSub := pickLatestSubdir(projectsRoot)
	var stop context.CancelFunc
	stop = startReporter(ctx, agent, currentSub, sidecarURL, interval)
	defer func() {
		if stop != nil {
			stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next := pickLatestSubdir(projectsRoot)
			if next != "" && next != currentSub {
				log.Printf("token-reporter: session dir rotated: %s -> %s", currentSub, next)
				if stop != nil {
					stop()
				}
				currentSub = next
				stop = startReporter(ctx, agent, currentSub, sidecarURL, interval)
			}
		}
	}
}

func nonEmptyOr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// pickLatestSubdir returns the subdir under root containing the .jsonl file
// with the newest mtime. Subdirs with no .jsonl are ignored. Returns "" if
// none exist.
//
// We pick by newest .jsonl content, not by dir mtime. Dir mtime only changes
// on entry create/delete/rename — not on writes to existing files — so an
// actively-appended session log does not bump its parent's mtime. Meanwhile
// unrelated filesystem ops on sibling dirs under projects/ (touch, mkdir,
// ln) do bump their mtime, which used to steal the pick and leave the
// reporter polling an empty dir until the pod restarted.
func pickLatestSubdir(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var best string
	var bestMtime time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(root, e.Name())
		subEntries, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range subEntries {
			if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if best == "" || info.ModTime().After(bestMtime) {
				best = sub
				bestMtime = info.ModTime()
			}
		}
	}
	return best
}

// startReporter spawns a Reporter AND an ActivityDetector against the
// given dir, returning a cancel func that stops both. When dir is "" no
// goroutines are started (no data yet — the outer loop retries every
// 60s and re-calls startReporter when the dir appears).
//
// Activity detection (kyber#249) lives here rather than in the
// status-sidecar container because the JSONL transcript is inside the
// agent's chroot/overlay-mounted filesystem; the sidecar is in its own
// filesystem namespace and can't see it. token-reporter is already in
// the right place + already parsing the same file.
func startReporter(ctx context.Context, agent, dir, sidecarURL string, interval time.Duration) context.CancelFunc {
	if dir == "" {
		log.Printf("token-reporter: no session subdir yet; will check again in 60s")
		return func() {}
	}
	rctx, cancel := context.WithCancel(ctx)
	rep := &tokenreport.Reporter{
		AgentName:   agent,
		ProjectsDir: dir,
		SidecarURL:  sidecarURL,
		Interval:    interval,
	}
	go rep.Run(rctx)
	det := &tokenreport.ActivityDetector{
		AgentName:   agent,
		ProjectsDir: dir,
		SidecarURL:  sidecarURL,
		// kyber#249 architecture: per-runtime detectors POST to the
		// platform sidecar's localhost forwarder; sidecar applies
		// agent identity + auth + onward delivery to control plane.
		// kyber#257 unified the egress: same SidecarURL across reporter,
		// credential-syncer, and activity detector.
		// Interval defaults to 1s inside ActivityDetector.Run.
	}
	go det.Run(rctx)
	log.Printf("token-reporter: started for agent=%s dir=%s interval=%s (+activity detector @ 1s)", agent, dir, interval)
	return cancel
}
