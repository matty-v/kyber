package main

import (
	"os"
	"path/filepath"
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
	s := newResourceSampler(t.TempDir(), filepath.Join(dir, "memory.events"), allocation)
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
	s := newResourceSampler(t.TempDir(), "", 1024)
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
		walk: func(string) (int64, error) {
			close(started)
			<-release
			return 1900 * 1024 * 1024, nil
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
	used, err := directoryUsage(persist)
	if err != nil {
		t.Fatal(err)
	}
	if used >= 1024*1024 {
		t.Fatalf("directory usage = %d; parent filesystem data was included", used)
	}
	usage := diskUsage(2*1024*1024*1024, used, "directory", "ready", time.Unix(100, 0))
	if usage.DiskTotalBytes != 2*1024*1024*1024 {
		t.Fatalf("disk total = %d, want 2Gi allocation", usage.DiskTotalBytes)
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
