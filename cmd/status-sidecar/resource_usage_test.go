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

	s := newResourceSampler(t.TempDir(), filepath.Join(dir, "memory.events"))
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
	if first.DiskTotalBytes <= 0 || first.DiskUsedBytes < 0 {
		t.Errorf("disk sample = used %d total %d", first.DiskUsedBytes, first.DiskTotalBytes)
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
	s := newResourceSampler(t.TempDir(), "")
	if _, err := s.sample(time.Now()); err == nil {
		t.Fatal("sample succeeded without a pod cgroup path")
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
