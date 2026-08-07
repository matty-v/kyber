package tokenreport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// pushRecorder captures what the syncer POSTs to the sidecar.
type pushRecorder struct {
	mu     sync.Mutex
	bodies []string
	status int
}

func (p *pushRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AuthJSON string `json:"auth_json"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.bodies = append(p.bodies, body.AuthJSON)
		p.mu.Unlock()
		code := p.status
		if code == 0 {
			code = http.StatusNoContent
		}
		w.WriteHeader(code)
	}
}

func (p *pushRecorder) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bodies...)
}

func writeAuth(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

const (
	origCred    = `{"auth_mode":"chatgpt","tokens":{"refresh_token":"ORIGINAL"}}`
	rotatedCred = `{"auth_mode":"chatgpt","tokens":{"refresh_token":"ROTATED"}}`
)

// TestCodexCredentialSyncer_PushesRotatedCredential is the core behaviour:
// when the CLI rotates the refresh token, the new document must reach the
// control plane so the Secret stops holding a burnt token (kyber#681).
func TestCodexCredentialSyncer_PushesRotatedCredential(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	writeAuth(t, authPath, origCred)

	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &CodexCredentialSyncer{AuthPath: authPath, SidecarURL: srv.URL, Interval: 50 * time.Millisecond}
	go s.Run(ctx)

	// Give the syncer a moment to seed its baseline from the boot-time file,
	// then simulate the CLI rotating the credential.
	time.Sleep(150 * time.Millisecond)
	writeAuth(t, authPath, rotatedCred)

	if !waitFor(t, 3*time.Second, func() bool { return len(rec.got()) > 0 }) {
		t.Fatal("syncer never pushed the rotated credential")
	}
	got := rec.got()
	if got[len(got)-1] != rotatedCred {
		t.Fatalf("pushed %q, want %q", got[len(got)-1], rotatedCred)
	}
}

// TestCodexCredentialSyncer_SkipsBootCredential guards against re-pushing the
// document the pod just booted with — that would write to the Secret on every
// single boot for no reason.
func TestCodexCredentialSyncer_SkipsBootCredential(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	writeAuth(t, authPath, origCred)

	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &CodexCredentialSyncer{AuthPath: authPath, SidecarURL: srv.URL, Interval: 30 * time.Millisecond}
	go s.Run(ctx)

	// Several poll intervals with an unchanged file must produce no traffic.
	time.Sleep(300 * time.Millisecond)
	if n := len(rec.got()); n != 0 {
		t.Fatalf("pushed %d times for an unchanged credential, want 0", n)
	}
}

// TestCodexCredentialSyncer_PushesInitialDeviceCredential covers the handoff
// from device login: auth.json is valid before the reporter starts, but the
// Kubernetes Secret still contains only Kyber's {} marker.
func TestCodexCredentialSyncer_PushesInitialDeviceCredential(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	writeAuth(t, authPath, origCred)

	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &CodexCredentialSyncer{
		AuthPath: authPath, SidecarURL: srv.URL, Interval: time.Hour, PushInitial: true,
	}
	go s.Run(ctx)

	if !waitFor(t, 3*time.Second, func() bool { return len(rec.got()) == 1 }) {
		t.Fatalf("initial device credential was not pushed; got %d pushes", len(rec.got()))
	}
	if got := rec.got()[0]; got != origCred {
		t.Fatalf("pushed %q, want %q", got, origCred)
	}
}

// TestCodexCredentialSyncer_SkipsMalformed ensures a torn write never
// overwrites a good Secret with junk.
func TestCodexCredentialSyncer_SkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	writeAuth(t, authPath, origCred)

	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &CodexCredentialSyncer{AuthPath: authPath, SidecarURL: srv.URL, Interval: 30 * time.Millisecond}
	go s.Run(ctx)

	time.Sleep(100 * time.Millisecond)
	writeAuth(t, authPath, `{"auth_mode":"chatgpt","tok`) // truncated

	time.Sleep(300 * time.Millisecond)
	if n := len(rec.got()); n != 0 {
		t.Fatalf("pushed %d times for a malformed file, want 0", n)
	}

	// ...and once the write completes, it does push.
	writeAuth(t, authPath, rotatedCred)
	if !waitFor(t, 3*time.Second, func() bool { return len(rec.got()) > 0 }) {
		t.Fatal("syncer did not recover after a malformed intermediate write")
	}
}

// TestCodexCredentialSyncer_SkipsWhenFileIsMissing covers api-key agents and
// the pre-login window: a missing auth.json is a silent no-op, never an error
// loop.
func TestCodexCredentialSyncer_SkipsWhenFileIsMissing(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json") // never created

	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &CodexCredentialSyncer{AuthPath: authPath, SidecarURL: srv.URL, Interval: 30 * time.Millisecond}
	go s.Run(ctx)

	time.Sleep(200 * time.Millisecond)
	if n := len(rec.got()); n != 0 {
		t.Fatalf("pushed %d times with no auth.json, want 0", n)
	}
}

// TestCodexCredentialSyncer_RetriesAfterServerError pins that a failed push is
// retried rather than silently dropped — losing a rotated token is precisely
// the failure this whole mechanism exists to prevent.
func TestCodexCredentialSyncer_RetriesAfterServerError(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	writeAuth(t, authPath, origCred)

	rec := &pushRecorder{status: http.StatusInternalServerError}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &CodexCredentialSyncer{AuthPath: authPath, SidecarURL: srv.URL, Interval: 30 * time.Millisecond}
	go s.Run(ctx)

	time.Sleep(100 * time.Millisecond)
	writeAuth(t, authPath, rotatedCred)

	// The first attempt 500s; the syncer must not treat that as success and
	// must come back with the same payload.
	if !waitFor(t, 5*time.Second, func() bool { return len(rec.got()) >= 2 }) {
		t.Fatalf("expected a retry after a 500, got %d attempt(s)", len(rec.got()))
	}
	for _, b := range rec.got() {
		if b != rotatedCred {
			t.Fatalf("retried with %q, want the rotated credential %q", b, rotatedCred)
		}
	}
}
