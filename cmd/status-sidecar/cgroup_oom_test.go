package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeMemoryEvents(t *testing.T, dir string, oomKill uint64) string {
	t.Helper()
	path := filepath.Join(dir, "memory.events.local")
	body := "low 0\nhigh 0\nmax 0\noom 0\noom_kill " +
		// strconv.FormatUint inlined to avoid a tiny import for one use
		formatUint(oomKill) +
		"\noom_group_kill 0\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func formatUint(n uint64) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	return digits
}

func TestReadOOMKillCount_ParsesCgroupV2Format(t *testing.T) {
	dir := t.TempDir()
	path := writeMemoryEvents(t, dir, 7)

	got, err := readOOMKillCount(path)
	if err != nil {
		t.Fatalf("readOOMKillCount: %v", err)
	}
	if got != 7 {
		t.Errorf("got %d, want 7", got)
	}
}

func TestReadOOMKillCount_FileMissing(t *testing.T) {
	_, err := readOOMKillCount("/nonexistent/path/to/memory.events.local")
	if err == nil {
		t.Fatal("expected error on missing file")
	}
}

func TestReadOOMKillCount_KeyAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.events.local")
	// File exists but has no oom_kill line — older cgroup version.
	if err := os.WriteFile(path, []byte("low 0\nhigh 0\nmax 0\noom 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readOOMKillCount(path)
	if err == nil {
		t.Fatal("expected error when oom_kill key is absent")
	}
	if !strings.Contains(err.Error(), "oom_kill") {
		t.Errorf("error should mention oom_kill: %v", err)
	}
}

// TestOOMDetector_BaselineThenIncrementPosts pins the core kyber#285
// behavior: first tick establishes a baseline (no post even if counter
// is non-zero), subsequent tick with a higher counter posts exactly
// one memory_oom event.
func TestOOMDetector_BaselineThenIncrementPosts(t *testing.T) {
	dir := t.TempDir()
	path := writeMemoryEvents(t, dir, 0)

	var posts atomic.Int32
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		lastBody = string(buf[:n])
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := config{
		AgentName:              "test",
		ControlPlaneURL:        srv.URL,
		CgroupMemoryEventsPath: path,
	}
	d := newOOMDetector(path)
	client := &http.Client{Timeout: postTimeout}
	logf := func(string, ...any) {}
	warnf := func(string, ...any) {}

	// Tick 1 — baseline (0). No post.
	if d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("baseline tick should not post")
	}
	if posts.Load() != 0 {
		t.Fatalf("baseline tick posted %d events, want 0", posts.Load())
	}

	// Tick 2 — counter unchanged (still 0). No post.
	if d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("unchanged tick should not post")
	}
	if posts.Load() != 0 {
		t.Fatalf("unchanged tick posted %d events, want 0", posts.Load())
	}

	// Counter increments to 1. Tick 3 — should post.
	writeMemoryEvents(t, dir, 1)
	if !d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("increment tick should post")
	}
	if posts.Load() != 1 {
		t.Fatalf("increment tick posted %d events, want 1", posts.Load())
	}
	if !strings.Contains(lastBody, `"type":"memory_oom"`) {
		t.Errorf("body should contain type=memory_oom: %s", lastBody)
	}
	if !strings.Contains(lastBody, `"oomKillCount":1`) {
		t.Errorf("body should contain oomKillCount=1: %s", lastBody)
	}

	// Tick 4 — counter still 1, no further post.
	if d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("unchanged-after-increment tick should not post")
	}
	if posts.Load() != 1 {
		t.Fatalf("post-increment tick re-posted; got %d events, want 1", posts.Load())
	}
}

// TestOOMDetector_NonZeroBaselineDoesNotPost pins the no-spurious-post
// behavior when the sidecar starts AFTER an OOM has already happened.
// The first read returns a non-zero counter; we treat that as baseline
// and do NOT attribute it to a fresh sidecar session.
func TestOOMDetector_NonZeroBaselineDoesNotPost(t *testing.T) {
	dir := t.TempDir()
	path := writeMemoryEvents(t, dir, 5) // pre-existing kills

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := config{
		AgentName:              "test",
		ControlPlaneURL:        srv.URL,
		CgroupMemoryEventsPath: path,
	}
	d := newOOMDetector(path)
	client := &http.Client{Timeout: postTimeout}
	logf := func(string, ...any) {}
	warnf := func(string, ...any) {}

	// Baseline tick — counter is 5 but it's our first observation. No post.
	if d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("non-zero-baseline tick should not post")
	}
	if posts.Load() != 0 {
		t.Fatalf("non-zero-baseline tick posted %d events, want 0", posts.Load())
	}

	// Counter increments to 6 → genuine new kill, should post.
	writeMemoryEvents(t, dir, 6)
	if !d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("post-baseline increment should post")
	}
	if posts.Load() != 1 {
		t.Fatalf("post-baseline increment posted %d events, want 1", posts.Load())
	}
}

// TestOOMDetector_ReadFailureIsLoggedOnceAndSwallowed pins the
// degraded-mode behavior: a missing/unreadable cgroup file logs once
// and continues — never crashes the heartbeat loop.
func TestOOMDetector_ReadFailureIsLoggedOnceAndSwallowed(t *testing.T) {
	d := newOOMDetector("/nonexistent/memory.events")
	cfg := config{AgentName: "test", ControlPlaneURL: "http://nowhere"}
	client := &http.Client{}

	var warnCount atomic.Int32
	logf := func(string, ...any) {}
	warnf := func(string, ...any) { warnCount.Add(1) }

	// 5 ticks — should all return false (no post) and warn exactly once.
	for i := 0; i < 5; i++ {
		if d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
			t.Fatalf("tick %d on missing cgroup should not post", i)
		}
	}
	if got := warnCount.Load(); got != 1 {
		t.Errorf("expected exactly 1 warn for missing cgroup across 5 ticks, got %d", got)
	}
}

// TestOOMDetector_EmptyPathIsDormantNoOp pins the disabled-mode
// behavior: when the path is empty (cgroup-derivation failed at
// startup), every tick returns false silently — no read, no warn.
func TestOOMDetector_EmptyPathIsDormantNoOp(t *testing.T) {
	d := newOOMDetector("")
	cfg := config{AgentName: "test", ControlPlaneURL: "http://nowhere"}
	client := &http.Client{}

	var warnCount, logCount atomic.Int32
	logf := func(string, ...any) { logCount.Add(1) }
	warnf := func(string, ...any) { warnCount.Add(1) }

	for i := 0; i < 10; i++ {
		if d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
			t.Fatalf("tick %d with empty path should never post", i)
		}
	}
	if got := warnCount.Load(); got != 0 {
		t.Errorf("expected no warn for disabled-mode detector, got %d", got)
	}
	if got := logCount.Load(); got != 0 {
		t.Errorf("expected no log for disabled-mode detector, got %d", got)
	}
}

// TestOOMDetector_PostFailureDoesNotBumpLastCount pins the retry
// behavior chewie flagged in the #296 review: when the CP is down
// during an OOM kill, the post fails — the detector must NOT bump
// lastCount, so the next tick (after CP recovers) re-tries the post.
func TestOOMDetector_PostFailureDoesNotBumpLastCount(t *testing.T) {
	dir := t.TempDir()
	path := writeMemoryEvents(t, dir, 0)

	var cpDown atomic.Bool
	cpDown.Store(true) // start with CP unreachable
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if cpDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := config{
		AgentName:              "test",
		ControlPlaneURL:        srv.URL,
		CgroupMemoryEventsPath: path,
	}
	d := newOOMDetector(path)
	client := &http.Client{Timeout: postTimeout}
	logf := func(string, ...any) {}
	var warnCount atomic.Int32
	warnf := func(string, ...any) { warnCount.Add(1) }

	// Baseline tick (counter = 0, no post).
	d.tick(context.Background(), client, cfg, time.Now(), logf, warnf)

	// OOM kill happens; counter goes to 1. CP is down, post fails.
	writeMemoryEvents(t, dir, 1)
	if d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("tick should have returned false on post failure")
	}
	if posts.Load() != 0 {
		t.Fatalf("expected 0 successful posts while CP is down, got %d", posts.Load())
	}

	// CP recovers. The retry tick should re-post (lastCount wasn't bumped
	// on the prior failure, so the increment is still visible).
	cpDown.Store(false)
	if !d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("retry tick after CP recovery should have posted")
	}
	if posts.Load() != 1 {
		t.Fatalf("expected exactly 1 post after CP recovery, got %d", posts.Load())
	}

	// Subsequent ticks — counter unchanged, no further posts.
	if d.tick(context.Background(), client, cfg, time.Now(), logf, warnf) {
		t.Fatal("post-recovery unchanged tick should not re-post")
	}
	if posts.Load() != 1 {
		t.Fatalf("expected stable at 1 post, got %d", posts.Load())
	}
}

// TestReadOOMKillCount_MalformedValueReturnsError pins the parse-error
// path: a non-numeric oom_kill value (corrupt cgroup file) should
// surface as an error rather than silently being treated as zero.
func TestReadOOMKillCount_MalformedValueReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.events")
	if err := os.WriteFile(path, []byte("oom_kill not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readOOMKillCount(path)
	if err == nil {
		t.Fatal("expected parse error on non-numeric oom_kill value")
	}
}

// TestParsePodCgroupPath_StripsContainerScope pins the kyber#285 path-
// derivation: given /proc/self/cgroup body, strip the trailing
// container-scope segment to get the pod-level cgroup path.
func TestParsePodCgroupPath_StripsContainerScope(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "containerd default kubelet (production shape)",
			in:   "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podc1a17255_4689_4866_9513_d91aa7c5c458.slice/cri-containerd-7cd1caca567eeca6419da1186bc5a8f2def4e87d37d5e8b624c04c4f6b36be08.scope",
			want: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podc1a17255_4689_4866_9513_d91aa7c5c458.slice",
		},
		{
			name: "cgroupfs driver",
			in:   "0::/kubepods/pod-deadbeef/container-cafef00d",
			want: "/kubepods/pod-deadbeef",
		},
		{
			name: "trailing whitespace",
			in:   "0::/kubepods.slice/pod.slice/cri-containerd-x.scope\n",
			want: "/kubepods.slice/pod.slice",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePodCgroupPath(tt.in)
			if err != nil {
				t.Fatalf("parsePodCgroupPath: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParsePodCgroupPath_NamespacedRootIsError pins the failure mode
// for clusters where cgroup namespacing is enabled — /proc/self/cgroup
// reports just "/" or empty, and we can't derive a pod path. Caller
// treats this as "OOM detection unavailable" not as a crash.
func TestParsePodCgroupPath_NamespacedRootIsError(t *testing.T) {
	cases := []string{
		"0::/",
		"0::",
	}
	for _, in := range cases {
		_, err := parsePodCgroupPath(in)
		if err == nil {
			t.Errorf("input %q should error (namespaced root)", in)
		}
	}
}

// TestParsePodCgroupPath_NoV2LineIsError pins cgroup-v1 / hybrid
// systems: a /proc/self/cgroup body with no "0::" line should error
// rather than silently misparsing a v1 numbered line.
func TestParsePodCgroupPath_NoV2LineIsError(t *testing.T) {
	v1Body := "1:cpu:/kubepods/pod-foo/cont-bar\n2:memory:/kubepods/pod-foo/cont-bar\n"
	_, err := parsePodCgroupPath(v1Body)
	if err == nil {
		t.Fatal("v1-only cgroup body should error")
	}
}

// TestResolvePodCgroupEventsPath_HappyPath end-to-ends the resolver
// against a synthetic /proc/self/cgroup file.
func TestResolvePodCgroupEventsPath_HappyPath(t *testing.T) {
	dir := t.TempDir()
	procPath := filepath.Join(dir, "self_cgroup")
	body := "0::/kubepods.slice/pod-uid.slice/cri-containerd-abc.scope"
	if err := os.WriteFile(procPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePodCgroupEventsPath(procPath, "/sys/fs/cgroup")
	if err != nil {
		t.Fatalf("resolvePodCgroupEventsPath: %v", err)
	}
	want := "/sys/fs/cgroup/kubepods.slice/pod-uid.slice/memory.events"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
