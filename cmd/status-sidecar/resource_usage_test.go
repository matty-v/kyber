package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResourceSampler(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.events", "oom_kill 0\n")
	write("memory.current", "1048576\n")
	write("memory.max", "2097152\n")
	write("cpu.stat", "usage_usec 1000000\n")
	write("cpu.max", "200000 100000\n")

	const allocation = int64(2 * 1024 * 1024 * 1024)
	s := newResourceSampler(t.TempDir(), filepath.Join(dir, "memory.events"), allocation, nil, false)
	s.disk.method = "statfs"
	start := time.Unix(100, 0)
	first, err := s.sample(start)
	if err != nil {
		t.Fatal(err)
	}
	if first.CPUUsageMillicores != 0 {
		t.Errorf("first CPU sample = %v, want 0", first.CPUUsageMillicores)
	}
	if first.CPULimitMillicores == nil || *first.CPULimitMillicores != 2000 {
		t.Errorf("CPU limit = %v, want 2000m", first.CPULimitMillicores)
	}
	if first.MemoryUsedBytes != 1048576 || first.MemoryLimitBytes == nil || *first.MemoryLimitBytes != 2097152 {
		t.Errorf("memory sample = used %d limit %v", first.MemoryUsedBytes, first.MemoryLimitBytes)
	}
	if first.DiskTotalBytes != allocation || first.DiskUsedBytes < 0 {
		t.Errorf("disk sample = used %d total %d", first.DiskUsedBytes, first.DiskTotalBytes)
	}
	if first.DiskUsageMethod != "statfs" || first.DiskUsageState != "ready" {
		t.Errorf("disk accounting = %s/%s, want statfs/ready", first.DiskUsageMethod, first.DiskUsageState)
	}

	write("cpu.stat", "usage_usec 2500000\n")
	second, err := s.sample(start.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if second.CPUUsageMillicores != 1500 {
		t.Errorf("CPU usage = %v, want 1500m", second.CPUUsageMillicores)
	}
}

func TestUnlimitedCgroupValues(t *testing.T) {
	dir := t.TempDir()
	memoryPath := filepath.Join(dir, "memory.max")
	cpuPath := filepath.Join(dir, "cpu.max")
	if err := os.WriteFile(memoryPath, []byte("max\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cpuPath, []byte("max 100000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	memory, err := readOptionalInt64File(memoryPath)
	if err != nil || memory != nil {
		t.Fatalf("memory max = %v, %v; want nil, nil", memory, err)
	}
	cpu, err := readCPULimit(cpuPath)
	if err != nil || cpu != nil {
		t.Fatalf("CPU max = %v, %v; want nil, nil", cpu, err)
	}
}

func TestResourceSamplerUnavailableCgroup(t *testing.T) {
	s := newResourceSampler(t.TempDir(), "", 1024, nil, false)
	if _, err := s.sample(time.Now()); err == nil {
		t.Fatal("sample succeeded without a pod cgroup path")
	}
}

func TestMountRootDistinguishesWholeAndBindMounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	body := "36 25 8:32 / /persist rw,relatime - ext4 /dev/sdc rw\n" +
		"37 25 8:32 /var/lib/rancher/k3s/storage/pvc-123 /persist-bind rw,relatime - ext4 /dev/sdc rw\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := mountRoot(path, "/persist")
	if err != nil || root != "/" {
		t.Fatalf("whole-volume mount root = %q, %v; want /", root, err)
	}
	root, err = mountRoot(path, "/persist-bind")
	if err != nil || root != "/var/lib/rancher/k3s/storage/pvc-123" {
		t.Fatalf("bind mount root = %q, %v", root, err)
	}
}

func TestDirectoryDiskSampleIsAsyncAndUsesAllocation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := &diskSampler{
		path:       "/persist",
		totalBytes: 2 * 1024 * 1024 * 1024,
		method:     "directory",
		state:      "pending",
		walk: func(string) (int64, bool, error) {
			close(started)
			<-release
			return 1900 * 1024 * 1024, false, nil
		},
	}
	now := time.Unix(100, 0)
	before := time.Now()
	first, err := s.sample(now)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(before) > 100*time.Millisecond {
		t.Fatal("directory sample blocked the heartbeat")
	}
	if first.DiskTotalBytes != 2*1024*1024*1024 || first.DiskUsageState != "pending" {
		t.Fatalf("first sample = %+v", first)
	}
	<-started
	close(release)
	for s.running.Load() {
		time.Sleep(time.Millisecond)
	}
	second, err := s.sample(now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if second.DiskUsedBytes != 1900*1024*1024 || second.DiskUsageState != "ready" {
		t.Fatalf("completed sample = %+v", second)
	}
	if !second.DiskReserveReached {
		t.Error("90%% reserve was not derived from the 2Gi allocation")
	}
}

func TestDirectoryDiskPartialSampleWarnsAndCanReachReserve(t *testing.T) {
	var warning string
	s := &diskSampler{
		path:       "/persist",
		totalBytes: 100,
		method:     "directory",
		state:      "pending",
		lastStart:  time.Unix(100, 0),
		walk:       func(string) (int64, bool, error) { return 95, true, nil },
		warn: func(message string, _ ...any) {
			warning = message
		},
	}
	s.scan(time.Unix(100, 0))
	usage, err := s.sample(time.Unix(101, 0))
	if err != nil {
		t.Fatal(err)
	}
	if usage.DiskUsageState != "partial" || !usage.DiskReserveReached {
		t.Fatalf("partial sample = %+v, want reserve reached", usage)
	}
	if !strings.Contains(warning, "skipped unreadable paths") {
		t.Fatalf("warning = %q", warning)
	}
}

func TestDirectoryDiskFailedSampleWarns(t *testing.T) {
	var warning string
	s := &diskSampler{
		path:       "/persist",
		totalBytes: 100,
		method:     "directory",
		state:      "pending",
		walk:       func(string) (int64, bool, error) { return 0, false, errors.New("read failed") },
		warn: func(message string, _ ...any) {
			warning = message
		},
	}
	s.scan(time.Unix(100, 0))
	if s.state != "error" {
		t.Fatalf("state = %q, want error", s.state)
	}
	if !strings.Contains(warning, "accounting failed") {
		t.Fatalf("warning = %q", warning)
	}
}

func TestDirectoryUsageExcludesLargerParentFilesystem(t *testing.T) {
	parent := t.TempDir()
	persist := filepath.Join(parent, "pvc-agent")
	if err := os.Mkdir(persist, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "other-agent"), make([]byte, 4*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persist, "agent-data"), make([]byte, 128*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	used, partial, err := directoryUsage(persist)
	if err != nil {
		t.Fatal(err)
	}
	if partial {
		t.Fatal("readable tree reported a partial scan")
	}
	if used >= 1024*1024 {
		t.Fatalf("directory usage = %d; parent filesystem data was included", used)
	}
	usage := diskUsage(2*1024*1024*1024, used, "directory", "ready", time.Unix(100, 0))
	if usage.DiskTotalBytes != 2*1024*1024*1024 {
		t.Fatalf("disk total = %d, want 2Gi allocation", usage.DiskTotalBytes)
	}
}

func TestDirectoryUsageSkipsRootOwnedUnreadableDirectory(t *testing.T) {
	if os.Getenv("MAT14_NONROOT_HELPER") == "1" {
		used, partial, err := directoryUsage(os.Getenv("MAT14_PERSIST_PATH"))
		if err != nil {
			t.Fatal(err)
		}
		if !partial {
			t.Fatal("non-root walk did not report unreadable directory")
		}
		if used <= 0 {
			t.Fatal("non-root walk discarded all reachable usage")
		}
		return
	}
	if os.Geteuid() != 0 {
		if _, err := os.ReadDir("/root"); !errors.Is(err, os.ErrPermission) {
			t.Skip("/root is not an unreadable root-owned directory in this environment")
		}
		used, partial, err := directoryUsage("/root")
		if err != nil {
			t.Fatal(err)
		}
		if !partial || used <= 0 {
			t.Fatalf("non-root walk = used %d partial %v, want usable partial result", used, partial)
		}
		return
	}
	persist, err := os.MkdirTemp("/tmp", "mat14-disk-walk-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(persist)
	if err := os.Chmod(persist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persist, "reachable"), make([]byte, 128*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(persist, "root-owned")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "secret"), make([]byte, 128*1024), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDirectoryUsageSkipsRootOwnedUnreadableDirectory$")
	cmd.Env = append(os.Environ(), "MAT14_NONROOT_HELPER=1", "MAT14_PERSIST_PATH="+persist)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("non-root helper failed: %v\n%s", err, output)
	}
}

func TestSyncDiskExhaustedMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "control", "disk-exhausted")
	if err := syncDiskExhaustedMarker(marker, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker not created: %v", err)
	}
	if err := syncDiskExhaustedMarker(marker, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker still exists after recovery: %v", err)
	}
}

// A partial sample must be able to REMOVE the marker, not only write it.
//
// This test previously asserted the opposite, under the name
// TestPartialDiskSampleCannotClearExhaustedMarker. That rule is what made
// DiskExhausted permanent: directory accounting is always partial on an agent
// with rootfs persistence, so the "ready sample proves recovery" escape it
// relied on never happened, and the marker kept the runtime session paused for
// the life of the volume. See diskreserve.ClearRatio.
//
// What must still hold is that a sample which measured NOTHING cannot move the
// marker in either direction, which the pending/error cases below cover.
func TestPartialDiskSampleClearsExhaustedMarkerOnRecovery(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "control", "disk-exhausted")
	if err := syncDiskExhaustedMarkerForSample(marker, "partial", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("partial sample did not write the marker: %v", err)
	}
	if err := syncDiskExhaustedMarkerForSample(marker, "partial", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("partial recovery left the runtime paused: %v", err)
	}
	if err := syncDiskExhaustedMarkerForSample(marker, "ready", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("ready recovery did not clear marker: %v", err)
	}
}

// pending and error mean no walk completed. Such a sample has no authority to
// pause or resume anything, so it must leave the marker exactly as it found it.
func TestUnmeasuredDiskSampleLeavesMarkerAlone(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "control", "disk-exhausted")
	if err := syncDiskExhaustedMarkerForSample(marker, "ready", true); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "error"} {
		if err := syncDiskExhaustedMarkerForSample(marker, state, false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("%s sample cleared an exhausted marker: %v", state, err)
		}
	}
	if err := syncDiskExhaustedMarkerForSample(marker, "ready", false); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "error"} {
		if err := syncDiskExhaustedMarkerForSample(marker, state, true); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%s sample paused a healthy agent", state)
		}
	}
}

// The sampler carries its own previous decision, which is what the hysteresis
// band holds onto. Walking a real directory tree back down past the clear ratio
// must release the reserve; sitting inside the band must not flap it.
func TestDiskSamplerHysteresisAcrossSamples(t *testing.T) {
	dir := t.TempDir()
	const total = 1000

	var used int64
	s := &diskSampler{
		path:       dir,
		totalBytes: total,
		method:     "directory",
		state:      "partial",
		walk:       func(string) (int64, bool, error) { return used, true, nil },
	}

	step := func(bytes int64) resourceUsage {
		t.Helper()
		used = bytes
		s.mu.Lock()
		s.usedBytes, s.sampledAt, s.state = bytes, time.Now(), "partial"
		s.mu.Unlock()
		return s.decide(diskUsage(total, bytes, "directory", "partial", time.Now()))
	}

	if u := step(950); !u.DiskReserveReached {
		t.Fatalf("95%% of the allocation did not trip the reserve: %+v", u)
	}
	if u := step(850); !u.DiskReserveReached {
		t.Fatalf("85%% is inside the hysteresis band and must hold the previous decision: %+v", u)
	}
	if u := step(750); u.DiskReserveReached {
		t.Fatalf("75%% is below the clear ratio and must release the reserve: %+v", u)
	}
	if u := step(850); u.DiskReserveReached {
		t.Fatalf("85%% after recovery must hold released, not re-trip: %+v", u)
	}
}

// A sidecar container restart must not resume a runtime the control plane still
// considers exhausted.
//
// The sidecar is a native sidecar with RestartPolicy:Always, so the kubelet can
// restart it on its own while the pod and the agent container carry on. Its
// hysteresis state is in memory; the pause marker on the shared runtime-control
// emptyDir is not. A sample inside the hold band resolves to whatever the
// previous decision was, so a sampler that restarts believing "not exhausted"
// decides false at 85%, removes the marker, and un-pauses the runtime — while
// the control plane, working from the Agent's durable status, still reports
// DiskExhausted. Seeding from the marker is what keeps the two callers agreeing.
func TestDiskSamplerSeedsHysteresisFromPauseMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "control", "disk-exhausted")
	if err := syncDiskExhaustedMarker(marker, true); err != nil {
		t.Fatal(err)
	}

	const total = 1000
	const inBand = 850 // 85%: between ClearRatio and TripRatio

	newSampler := func(seed bool) *diskSampler {
		s := newDiskSampler(t.TempDir(), "/proc/self/mountinfo", total, nil, seed)
		s.method = "directory"
		s.usedBytes, s.state, s.sampledAt = inBand, "partial", time.Now()
		return s
	}

	// Restarted WITHOUT the seed — the bug this guards.
	cold := newSampler(false)
	if u := cold.decide(diskUsage(total, inBand, "directory", "partial", time.Now())); u.DiskReserveReached {
		t.Fatal("unseeded sampler unexpectedly held exhausted; this test can no longer detect the regression")
	}

	// Restarted WITH the marker seed — what main.go now does.
	warm := newSampler(diskExhaustedMarkerPresent(marker))
	usage := warm.decide(diskUsage(total, inBand, "directory", "partial", time.Now()))
	if !usage.DiskReserveReached {
		t.Fatalf("restarted sidecar dropped the exhausted verdict inside the hold band: %+v", usage)
	}
	if err := syncDiskExhaustedMarkerForSample(marker, usage.DiskUsageState, usage.DiskReserveReached); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("restarted sidecar removed the pause marker and resumed the runtime: %v", err)
	}

	// And once usage genuinely falls below the clear ratio it still releases.
	released := warm.decide(diskUsage(total, 700, "directory", "partial", time.Now()))
	if released.DiskReserveReached {
		t.Fatalf("seeded sampler could not release below the clear ratio: %+v", released)
	}
	if err := syncDiskExhaustedMarkerForSample(marker, released.DiskUsageState, released.DiskReserveReached); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker survived a genuine recovery: %v", err)
	}
}

// An absent marker means the runtime is not paused — the state after a whole-pod
// recreation, where the emptyDir goes with the pod and the agent container comes
// back with a fresh session. Seeding false there is correct, not a gap.
func TestDiskSamplerSeedsHealthyWhenNoMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "control", "disk-exhausted")
	if diskExhaustedMarkerPresent(marker) {
		t.Fatal("no marker was written, but one was reported present")
	}
	s := newDiskSampler(t.TempDir(), "/proc/self/mountinfo", 1000, nil, diskExhaustedMarkerPresent(marker))
	if u := s.decide(diskUsage(1000, 850, "directory", "partial", time.Now())); u.DiskReserveReached {
		t.Fatalf("a fresh pod with no pause marker started out exhausted: %+v", u)
	}
}
