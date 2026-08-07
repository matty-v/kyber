package tokenreport_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
