// Activity detection for the kyber-status-sidecar pipeline (kyber#249).
// Lives in the runtime image (alongside kyber-token-reporter) because the
// sidecar can't see the agent's chroot/overlay-mounted filesystem from a
// separate container — the JSONL transcript is reachable here, not from
// the platform-level sidecar. See kyber#249 design discussion.
//
// The detector is intentionally tiny: a 1s tick reads the LAST line of
// the active session file and derives state from the same `usage.speed`
// signal kyber-token-reporter uses for "finalized" detection. POSTs only
// fire on state-change to the same /internal/agents/{name}/status-event
// endpoint the sidecar uses for heartbeats.

package tokenreport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
)

// Activity states. Kept as exported constants so external callers (the
// future PWA-side renderer) can reference them without re-spelling the
// strings.
const (
	ActivityWorking = "working"
	ActivityIdle    = "idle"
	ActivityUnknown = "unknown"
)

// activityIdleAfter is how long the file can sit untouched before we
// flip the state to "idle" even if the last line looks streaming. Covers
// the case where Claude crashed mid-stream and never wrote a finalized
// entry.
const activityIdleAfter = 5 * time.Second

// ActivityDetector is the runnable side of activity reporting. POSTs
// state-change events to the per-pod kyber-status-sidecar's localhost
// forwarder (http://127.0.0.1:8091/event by default). The sidecar
// applies agent name + auth + onward-delivery to the control plane.
//
// Per kyber#249's architecture pivot (2026-05-03 with Matt): runtime-
// specific binaries don't talk to the control plane directly anymore;
// they fan into the sidecar which is the sole in-pod-to-control-plane
// conduit for status events.
type ActivityDetector struct {
	AgentName   string
	ProjectsDir string
	// SidecarURL is the base URL of the sidecar's localhost forwarder, e.g.
	// "http://127.0.0.1:8091". Defaults to that exact value when zero.
	// activity events POST to {SidecarURL}/event (per-path appending was
	// introduced by kyber#257 so reporter / credential-syncer / detector
	// all share one base URL).
	SidecarURL string

	// Interval is the poll cadence. Defaults to 1s when zero.
	Interval time.Duration

	// HTTPClient is optional; defaults to http.Client with a 3s timeout.
	HTTPClient *http.Client

	// lastPushed remembers the most recently posted state so we only POST
	// on transitions. Atomic-ish via mutex (cheap; single-writer).
	mu         sync.Mutex
	lastPushed string

	// firstConnectSucceeded flips true on our first successful POST to the
	// sidecar's localhost forwarder. Until then, connection-refused errors
	// are treated as the expected sidecar-startup race (#254) and suppressed
	// after a single info-level log line.
	firstConnectSucceeded   bool
	loggedWaitingForSidecar bool
}

// Run blocks until ctx is cancelled, polling on d.Interval and POSTing
// on every state change. Errors are logged, never returned — the
// detector must never crash the agent pod (parity with Reporter.Run).
func (d *ActivityDetector) Run(ctx context.Context) {
	if d.Interval == 0 {
		d.Interval = time.Second
	}
	client := d.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}

	tick := time.NewTicker(d.Interval)
	defer tick.Stop()

	// Fire once immediately so the first state lands without waiting.
	_ = d.tick(ctx, client)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := d.tick(ctx, client); err != nil {
				log.Printf("[activity] tick error: %v", err)
			}
		}
	}
}

// tick reads the latest transcript, infers state, and POSTs if the
// state changed since the last push. Returns nil on "nothing to do."
func (d *ActivityDetector) tick(ctx context.Context, client *http.Client) error {
	state, lastAt, err := DetectActivity(d.ProjectsDir)
	if err != nil {
		// Treat any read error as "unknown" — push it once so the PWA
		// stops showing a stale state, but otherwise don't spam.
		state = ActivityUnknown
		lastAt = time.Now().UTC()
	}

	d.mu.Lock()
	prev := d.lastPushed
	d.mu.Unlock()
	if state == prev {
		return nil // no change
	}

	if err := d.post(ctx, client, state, lastAt); err != nil {
		// Sidecar startup race (#254): until our first successful POST,
		// connection-refused is the expected "sidecar's listener isn't
		// up yet" signal. Log a single info line then suppress so the
		// agent log doesn't fill with retry noise. After the first
		// success, all errors flow through normally so a real later
		// failure (sidecar crash) still surfaces.
		d.mu.Lock()
		firstOK := d.firstConnectSucceeded
		warned := d.loggedWaitingForSidecar
		d.mu.Unlock()
		if !firstOK && isConnRefused(err) {
			if !warned {
				d.mu.Lock()
				d.loggedWaitingForSidecar = true
				d.mu.Unlock()
				log.Printf("[activity] waiting for sidecar to come up at %s", d.sidecarURL())
			}
			return nil
		}
		return err
	}
	d.mu.Lock()
	d.lastPushed = state
	d.firstConnectSucceeded = true
	d.mu.Unlock()
	return nil
}

// sidecarURL is the resolved POST target. SidecarURL is the BASE URL
// (kyber#257); /event is appended here so callers / log lines all agree.
func (d *ActivityDetector) sidecarURL() string {
	base := d.SidecarURL
	if base == "" {
		base = "http://127.0.0.1:8091"
	}
	return base + "/event"
}

// isConnRefused reports whether err's underlying cause is ECONNREFUSED. The
// net package wraps syscall errors in *net.OpError, which Unwraps cleanly so
// errors.Is reaches the syscall errno. Used to detect the sidecar-startup
// race; any other error path goes through normal logging.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// post sends a single activity event to the sidecar's localhost
// forwarder. Wire shape (statusEvent) is identical to what the sidecar
// itself emits for heartbeats; sidecar adds agent name + auth + onward
// delivery to the control plane.
func (d *ActivityDetector) post(ctx context.Context, client *http.Client, state string, lastAt time.Time) error {
	body, err := json.Marshal(map[string]string{
		"type":  "activity",
		"state": state,
		"at":    lastAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.sidecarURL(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("sidecar returned %s", resp.Status)
	}
	return nil
}

// DetectActivity inspects the latest transcript in projectsDir and
// returns the current activity state plus the wall-clock of the most
// recent observed activity. Pure function — no HTTP, no globals.
//
// Decision logic:
//   - No transcript file → ActivityUnknown
//   - File hasn't been touched in activityIdleAfter → ActivityIdle (covers
//     mid-stream crash; the agent is no longer working even if the last
//     line still looks like it)
//   - Last assistant entry has speed != "?" / "" → ActivityIdle (turn
//     finalized)
//   - Last assistant entry has speed == "?" → ActivityWorking (streaming)
//   - Last entry is type=user → ActivityWorking (operator just sent a
//     message; assistant is about to respond)
//   - Anything else → ActivityUnknown (don't false-positive)
func DetectActivity(projectsDir string) (state string, lastAt time.Time, err error) {
	latest, ferr := FindLatestSessionFile(projectsDir)
	if ferr != nil {
		return ActivityUnknown, time.Time{}, nil
	}
	info, ierr := os.Stat(latest)
	if ierr != nil {
		return ActivityUnknown, time.Time{}, nil
	}
	mtime := info.ModTime()

	// File hasn't been touched in a while — treat as idle even if the
	// last line is streaming-shaped (covers crash-mid-stream).
	if time.Since(mtime) > activityIdleAfter {
		return ActivityIdle, mtime, nil
	}

	// Read the LAST non-empty JSON line. We don't need 50 lines like
	// ParseLatest does — activity only cares about the most recent
	// entry. Tail-read keeps memory bounded for large transcripts.
	last, lerr := readLastJSONLine(latest)
	if lerr != nil {
		return ActivityUnknown, mtime, nil
	}
	if last == "" {
		return ActivityUnknown, mtime, nil
	}

	var e rawEntry
	if uerr := json.Unmarshal([]byte(last), &e); uerr != nil {
		return ActivityUnknown, mtime, nil
	}

	switch e.Type {
	case "user":
		// Operator just typed something; the assistant is about to react.
		return ActivityWorking, mtime, nil
	case "assistant":
		speed := e.Message.Usage.Speed
		if speed == "?" || speed == "" {
			return ActivityWorking, mtime, nil
		}
		return ActivityIdle, mtime, nil
	}
	return ActivityUnknown, mtime, nil
}

// readLastJSONLine reads the last non-empty line from a JSONL file.
// Returns "" if the file is empty. Uses an O(N) scan for files smaller
// than 64 KB and a tail-from-end seek for larger files.
func readLastJSONLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	const tailWindow = 64 * 1024
	if info.Size() <= tailWindow {
		return readLastLineSimple(f)
	}
	// Seek to the last tailWindow bytes and scan from there. That
	// guarantees we capture the last line even on very long
	// transcripts without holding the whole file in memory.
	if _, err := f.Seek(info.Size()-tailWindow, io.SeekStart); err != nil {
		return "", err
	}
	return readLastLineSimple(f)
}

func readLastLineSimple(f *os.File) (string, error) {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var last string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		last = string(line)
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return last, nil
}
