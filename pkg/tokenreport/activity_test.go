package tokenreport

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeTranscript creates a fake Claude session file directly in dir.
// FindLatestSessionFile (used by DetectActivity) reads .jsonl files from
// the dir it's given without recursing — token-reporter's
// pickLatestSubdir handles the dir-walking before the detector runs.
// Returns the full path so tests can stat / mtime-tweak it.
func writeTranscript(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestDetectActivity_NoTranscript(t *testing.T) {
	dir := t.TempDir()
	state, _, err := DetectActivity(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != ActivityUnknown {
		t.Errorf("state: got %q, want %q", state, ActivityUnknown)
	}
}

func TestDetectActivity_StreamingAssistant_Working(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"user","message":{"role":"user"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"speed":"?"}}}` + "\n"
	writeTranscript(t, dir, body)
	state, _, err := DetectActivity(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != ActivityWorking {
		t.Errorf("state: got %q, want %q", state, ActivityWorking)
	}
}

func TestDetectActivity_FinalizedAssistant_Idle(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"user","message":{"role":"user"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"speed":"standard"}}}` + "\n"
	writeTranscript(t, dir, body)
	state, _, err := DetectActivity(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != ActivityIdle {
		t.Errorf("state: got %q, want %q", state, ActivityIdle)
	}
}

func TestDetectActivity_LastIsUser_Working(t *testing.T) {
	dir := t.TempDir()
	// Operator just typed something; assistant hasn't started yet.
	body := `{"type":"assistant","message":{"role":"assistant","usage":{"speed":"standard"}}}` + "\n" +
		`{"type":"user","message":{"role":"user"}}` + "\n"
	writeTranscript(t, dir, body)
	state, _, err := DetectActivity(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != ActivityWorking {
		t.Errorf("state: got %q, want %q", state, ActivityWorking)
	}
}

func TestDetectActivity_StaleFile_IdleEvenIfStreaming(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"assistant","message":{"role":"assistant","usage":{"speed":"?"}}}` + "\n"
	path := writeTranscript(t, dir, body)
	// Backdate the file past the idle threshold to simulate
	// crash-mid-stream.
	stale := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	state, _, err := DetectActivity(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != ActivityIdle {
		t.Errorf("state: got %q, want %q (file is stale; should override the streaming-shaped last line)", state, ActivityIdle)
	}
}

// TestActivityDetector_PostsToSidecarOnStateChange verifies the runtime
// binary fans into the sidecar's localhost forwarder rather than the
// control plane directly (kyber#249 architecture). Wire shape: same
// statusEvent the heartbeat loop emits.
func TestActivityDetector_PostsToSidecarOnStateChange(t *testing.T) {
	// Stand up a fake sidecar localhost forwarder.
	type recorded struct {
		path string
		body string
	}
	got := make(chan recorded, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- recorded{path: r.URL.Path, body: string(body)}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	dir := t.TempDir()
	body := `{"type":"assistant","message":{"role":"assistant","usage":{"speed":"?"}}}` + "\n"
	writeTranscript(t, dir, body)

	det := &ActivityDetector{
		AgentName:   "alice",
		ProjectsDir: dir,
		SidecarURL:  srv.URL,
		Interval:    20 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go det.Run(ctx)

	select {
	case rec := <-got:
		if rec.path != "/event" {
			t.Errorf("path: got %q, want /event", rec.path)
		}
		// Body should be {"type":"activity","state":"working","at":"..."}
		if !strings.Contains(rec.body, `"type":"activity"`) {
			t.Errorf("body missing type=activity: %s", rec.body)
		}
		if !strings.Contains(rec.body, `"state":"working"`) {
			t.Errorf("body missing state=working: %s", rec.body)
		}
	case <-time.After(800 * time.Millisecond):
		t.Fatal("no event posted to sidecar within 800ms")
	}
}

func TestDetectActivity_MalformedLastLine_Unknown(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "garbage\n")
	state, _, err := DetectActivity(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != ActivityUnknown {
		t.Errorf("state: got %q, want %q", state, ActivityUnknown)
	}
}

func TestDetectActivity_TailReadHandlesLargeFile(t *testing.T) {
	dir := t.TempDir()
	// Build a transcript larger than the 64 KB tailWindow so the
	// detector's "seek to last 64 KB" branch fires. Pad with system-
	// type entries that DetectActivity ignores; close with a finalized
	// assistant so the expected state is Idle.
	pad := `{"type":"system","message":{"role":"system"}}`
	bodyParts := make([]byte, 0, 70*1024)
	for len(bodyParts) < 70*1024 {
		bodyParts = append(bodyParts, []byte(pad+"\n")...)
	}
	bodyParts = append(bodyParts,
		[]byte(`{"type":"assistant","message":{"role":"assistant","usage":{"speed":"standard"}}}`+"\n")...)
	writeTranscript(t, dir, string(bodyParts))

	state, _, err := DetectActivity(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != ActivityIdle {
		t.Errorf("state: got %q, want %q (tail-read should find the finalized last line)", state, ActivityIdle)
	}
}

// TestActivityDetector_TickSuppressesStartupRaceConnRefused exercises the
// sidecar-startup-race fix from #254. Behavior contract:
//   - First connection-refused before any successful POST: log a single
//     "waiting for sidecar" line and return nil (no surfaced error).
//   - Subsequent connection-refused while still pre-success: silent.
//   - First successful POST: flips firstConnectSucceeded.
//   - Connection-refused AFTER a prior success: returned as an error so the
//     caller logs it normally (a real failure, not a startup race).
func TestActivityDetector_TickSuppressesStartupRaceConnRefused(t *testing.T) {
	dir := t.TempDir()
	// Two transcripts that map to working/idle so each tick() crosses the
	// state-change gate and actually calls post(). Helper rewrites the
	// session file in place.
	streaming := `{"type":"assistant","message":{"role":"assistant","usage":{"speed":"?"}}}` + "\n"
	finalized := `{"type":"assistant","message":{"role":"assistant","usage":{"speed":"standard"}}}` + "\n"
	writeTranscript(t, dir, streaming) // initial: working

	// Stand up an httptest.Server and immediately Close it — the URL stays
	// well-formed but every dial gets ECONNREFUSED. Reusable as the
	// "sidecar isn't up yet" target without flake-prone port games.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()

	// Capture log output so we can assert exactly one "waiting" line.
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	d := &ActivityDetector{
		AgentName:   "test",
		ProjectsDir: dir,
		SidecarURL:  dead.URL,
		HTTPClient:  &http.Client{Timeout: time.Second},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Tick 1 — ECONNREFUSED. Should swallow + log the waiting line.
	if err := d.tick(ctx, d.HTTPClient); err != nil {
		t.Fatalf("tick 1: got err %v, want nil (suppressed)", err)
	}
	// Force the next tick to cross the state gate again.
	writeTranscript(t, dir, finalized) // working -> idle
	d.mu.Lock()
	d.lastPushed = ActivityWorking // pretend the last push was idle's opposite
	d.mu.Unlock()

	// Tick 2 — ECONNREFUSED again, this time silent.
	if err := d.tick(ctx, d.HTTPClient); err != nil {
		t.Fatalf("tick 2: got err %v, want nil (suppressed)", err)
	}

	logged := buf.String()
	if c := strings.Count(logged, "waiting for sidecar"); c != 1 {
		t.Errorf("waiting log count: got %d, want 1; full log:\n%s", c, logged)
	}
	if strings.Contains(logged, "tick error") {
		t.Errorf("expected no [activity] tick error logs during startup race; got:\n%s", logged)
	}

	// Bring up a real sidecar that 204s. Tick 3 should succeed and flip
	// firstConnectSucceeded.
	var hits atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer live.Close()
	d.SidecarURL = live.URL

	// Cross the state gate one more time.
	writeTranscript(t, dir, streaming)
	d.mu.Lock()
	d.lastPushed = ActivityIdle
	d.mu.Unlock()

	if err := d.tick(ctx, d.HTTPClient); err != nil {
		t.Fatalf("tick 3 (live): got err %v, want nil", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("live sidecar hits: got %d, want 1", got)
	}
	d.mu.Lock()
	firstOK := d.firstConnectSucceeded
	d.mu.Unlock()
	if !firstOK {
		t.Error("firstConnectSucceeded should flip true after a successful POST")
	}

	// Tick 4 — back to a dead URL after a success: error must propagate
	// up so the operator sees a real later failure.
	d.SidecarURL = dead.URL
	writeTranscript(t, dir, finalized)
	d.mu.Lock()
	d.lastPushed = ActivityWorking
	d.mu.Unlock()
	if err := d.tick(ctx, d.HTTPClient); err == nil {
		t.Error("tick 4 (dead after success): got nil, want propagated error")
	}
}

// TestIsConnRefused locks the predicate behind the startup-race suppression.
// We synthesize the error with a real dial against an unused port so the
// test reflects what tick() actually sees from net/http.
func TestIsConnRefused(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	resp, err := http.Get(dead.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected dial error against closed server, got nil")
	}
	if !isConnRefused(err) {
		t.Errorf("isConnRefused(%v) = false, want true", err)
	}
	// Sanity: a generic non-network error is not connection-refused.
	if isConnRefused(io.EOF) {
		t.Error("isConnRefused(io.EOF) = true, want false")
	}
}
