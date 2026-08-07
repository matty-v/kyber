package tokenreport_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

func TestReporter_PostsParsedSnapshot(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/finalized.jsonl", filepath.Join(dir, "session.jsonl"))

	var count atomic.Int32
	var received tokenreport.Snapshot
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// kyber#257: reporter posts to the sidecar's localhost forwarder at
		// /token-usage; the sidecar adds the per-agent URL prefix on the
		// outbound hop. Tests stand a fake sidecar in srv's place.
		if r.URL.Path != "/token-usage" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("decode: %v", err)
		}
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rep := &tokenreport.Reporter{
		AgentName:   "test-agent",
		ProjectsDir: dir,
		SidecarURL:  srv.URL,
		Interval:    50 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rep.Run(ctx)

	if count.Load() < 1 {
		t.Fatalf("expected at least 1 POST, got %d", count.Load())
	}
	if received.Tokens.Used != 45231 {
		t.Errorf("Tokens.Used=%d want 45231", received.Tokens.Used)
	}
}

// TestReporter_AccumulatesOutputAcrossTicks pins the reporter-side cumulative
// output contract: content that predates reporter start is NOT counted
// (Output starts at 0), and messages appended while the reporter runs —
// including several within one tick window — are summed into
// Snapshot.Tokens.Output on subsequent POSTs. The pre-fix code reported only
// the newest finalized message's output, losing intermediates.
func TestReporter_AccumulatesOutputAcrossTicks(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "session.jsonl")
	copyFile(t, "testdata/finalized.jsonl", session)

	var mu sync.Mutex
	var snaps []tokenreport.Snapshot
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var s tokenreport.Snapshot
		if err := json.Unmarshal(body, &s); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		snaps = append(snaps, s)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rep := &tokenreport.Reporter{
		AgentName:   "test-agent",
		ProjectsDir: dir,
		SidecarURL:  srv.URL,
		Interval:    50 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { rep.Run(ctx); close(done) }()

	// Let a few ticks pass on the pre-existing transcript, then append TWO
	// finalized messages inside one tick window.
	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(session, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"assistant","uuid":"u-t1","message":{"id":"msg_t1","model":"claude-sonnet-4-5","role":"assistant","usage":{"input_tokens":12100,"cache_creation_input_tokens":8000,"cache_read_input_tokens":25231,"output_tokens":30,"speed":"standard","service_tier":"standard"}},"effortLevel":"medium"}` + "\n" +
		`{"type":"assistant","uuid":"u-t2","message":{"id":"msg_t2","model":"claude-sonnet-4-5","role":"assistant","usage":{"input_tokens":12200,"cache_creation_input_tokens":8000,"cache_read_input_tokens":25231,"output_tokens":40,"speed":"standard","service_tier":"standard"}},"effortLevel":"medium"}` + "\n"
	if _, err := f.WriteString(lines); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(snaps) < 2 {
		t.Fatalf("expected at least 2 POSTs, got %d", len(snaps))
	}
	if got := snaps[0].Tokens.Output; got != 0 {
		t.Errorf("first POST Output = %d, want 0 (pre-start transcript content is not counted)", got)
	}
	last := snaps[len(snaps)-1]
	if last.Tokens.Output != 70 {
		t.Errorf("last POST Output = %d, want 70 (30+40 — both appended messages summed)", last.Tokens.Output)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
