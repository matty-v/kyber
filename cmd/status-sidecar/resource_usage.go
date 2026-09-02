package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/matty-v/kyber/pkg/diskreserve"
)

const directoryScanInterval = 5 * time.Minute
const directoryScanYieldEvery = 256
const diskExhaustedMarker = "/var/run/kyber/disk-exhausted"

type resourceUsage struct {
	CPUUsageMillicores        int64  `json:"cpuUsageMillicores"`
	CPULimitMillicores        *int64 `json:"cpuLimitMillicores,omitempty"`
	MemoryUsedBytes           int64  `json:"memoryUsedBytes"`
	MemoryLimitBytes          *int64 `json:"memoryLimitBytes,omitempty"`
	DiskUsedBytes             int64  `json:"diskUsedBytes"`
	DiskTotalBytes            int64  `json:"diskTotalBytes"`
	DiskLimitEnforced         bool   `json:"diskLimitEnforced"`
	DiskBackingTotalBytes     int64  `json:"diskBackingTotalBytes,omitempty"`
	DiskBackingAvailableBytes int64  `json:"diskBackingAvailableBytes,omitempty"`
	DiskReserveReached        bool   `json:"diskReserveReached"`
	DiskUsageMethod           string `json:"diskUsageMethod"`
	DiskUsageState            string `json:"diskUsageState"`
	DiskUsedSampledAt         string `json:"diskUsedSampledAt,omitempty"`
}

func syncDiskExhaustedMarker(path string, reached bool) error {
	if reached {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("disk reserve reached\n"), 0o644)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// diskExhaustedMarkerPresent reports whether the runtime is currently paused,
// which is the sidecar's only durable record of its own previous verdict.
//
// The sidecar is a native sidecar with RestartPolicy:Always, so the kubelet
// restarts it independently of the pod (kyber#575). Its in-memory decision dies
// with the process while the pause marker — on the runtime-control emptyDir it
// shares with the agent container — does not. Without seeding from it, a
// restarted sidecar starts from "not exhausted", and a sample inside the
// hysteresis band then resolves to false and REMOVES the marker: the runtime
// resumes while the control plane, working from the Agent's durable status,
// still reports DiskExhausted.
//
// On whole-pod recreation the emptyDir goes with the pod and the runtime comes
// back unpaused, so an absent marker is the correct seed there too.
func diskExhaustedMarkerPresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func syncDiskExhaustedMarkerForSample(path, state string, reached bool) error {
	// "ready" and "partial" both carry a measured figure, and the hysteresis in
	// diskreserve.Decide has already resolved it into this boolean, so the
	// marker simply follows it. A partial sample used to be allowed to set this
	// marker but never to remove it, which left the runtime session paused for
	// the life of the volume on any agent whose walk is permanently partial —
	// see the ClearRatio comment for why that asymmetry cost more than it
	// bought.
	//
	// "pending" and "error" measured nothing, so they leave the marker exactly
	// as it is rather than asserting either state.
	switch state {
	case "ready", "partial":
		return syncDiskExhaustedMarker(path, reached)
	}
	return nil
}

type resourceSampler struct {
	cgroupPath   string
	previousCPU  uint64
	previousTime time.Time
	disk         *diskSampler
}

func newResourceSampler(persistPath, cgroupEventsPath string, diskTotalBytes int64, warn func(string, ...any), reserveReached bool) *resourceSampler {
	cgroupPath := ""
	if cgroupEventsPath != "" {
		cgroupPath = strings.TrimSuffix(cgroupEventsPath, "/memory.events")
	}
	return &resourceSampler{
		cgroupPath: cgroupPath,
		disk:       newDiskSampler(persistPath, "/proc/self/mountinfo", diskTotalBytes, warn, reserveReached),
	}
}

func (s *resourceSampler) sample(now time.Time) (resourceUsage, error) {
	usage, err := s.disk.sample(now)
	if err != nil {
		return resourceUsage{}, err
	}

	if s.cgroupPath == "" {
		return resourceUsage{}, fmt.Errorf("pod cgroup path unavailable")
	}
	memoryUsed, err := readInt64File(s.cgroupPath + "/memory.current")
	if err != nil {
		return resourceUsage{}, err
	}
	usage.MemoryUsedBytes = memoryUsed
	usage.MemoryLimitBytes, err = readOptionalInt64File(s.cgroupPath + "/memory.max")
	if err != nil {
		return resourceUsage{}, err
	}

	cpuUsec, err := readCPUUsageUsec(s.cgroupPath + "/cpu.stat")
	if err != nil {
		return resourceUsage{}, err
	}
	if !s.previousTime.IsZero() && now.After(s.previousTime) && cpuUsec >= s.previousCPU {
		usage.CPUUsageMillicores = int64(float64(cpuUsec-s.previousCPU) / float64(now.Sub(s.previousTime).Microseconds()) * 1000)
	}
	s.previousCPU = cpuUsec
	s.previousTime = now
	usage.CPULimitMillicores, err = readCPULimit(s.cgroupPath + "/cpu.max")
	if err != nil {
		return resourceUsage{}, err
	}
	return usage, nil
}

type diskSampler struct {
	path       string
	totalBytes int64
	method     string
	walk       func(string) (int64, bool, error)
	warn       func(string, ...any)

	mu        sync.Mutex
	usedBytes int64
	sampledAt time.Time
	state     string
	lastStart time.Time
	running   atomic.Bool

	// reserveReached is the sampler's own last decision, which is the previous
	// value the hysteresis band needs. The sidecar has no view of the Agent's
	// status, so a restarted process seeds this from the pause marker rather
	// than from false — see diskExhaustedMarkerPresent for what goes wrong
	// otherwise.
	reserveReached bool
}

func newDiskSampler(path, mountInfoPath string, totalBytes int64, warn func(string, ...any), reserveReached bool) *diskSampler {
	method := "directory"
	if root, err := mountRoot(mountInfoPath, path); err == nil && root == "/" {
		method = "statfs"
	}
	return &diskSampler{
		path:           path,
		totalBytes:     totalBytes,
		method:         method,
		walk:           directoryUsage,
		warn:           warn,
		state:          "pending",
		reserveReached: reserveReached,
	}
}

func (s *diskSampler) sample(now time.Time) (resourceUsage, error) {
	if s.totalBytes <= 0 {
		return resourceUsage{}, fmt.Errorf("disk allocation must be positive")
	}
	if s.method == "statfs" {
		backingTotal, backingAvailable, err := filesystemCapacity(s.path)
		if err != nil {
			return resourceUsage{}, err
		}
		usage := diskUsage(s.totalBytes, backingTotal-backingAvailable, "statfs", "ready", now)
		usage.DiskLimitEnforced = capacityBounded(s.totalBytes, backingTotal)
		usage.DiskBackingTotalBytes = backingTotal
		usage.DiskBackingAvailableBytes = backingAvailable
		return s.decide(usage), nil
	}

	s.mu.Lock()
	if !s.running.Load() && (s.lastStart.IsZero() || now.Sub(s.lastStart) >= directoryScanInterval) {
		s.lastStart = now
		s.running.Store(true)
		go s.scan(now)
	}
	used, sampledAt, state := s.usedBytes, s.sampledAt, s.state
	s.mu.Unlock()
	usage := diskUsage(s.totalBytes, used, "directory", state, sampledAt)
	if backingTotal, backingAvailable, err := filesystemCapacity(s.path); err == nil {
		usage.DiskBackingTotalBytes = backingTotal
		usage.DiskBackingAvailableBytes = backingAvailable
	} else if s.warn != nil {
		s.warn("disk backing filesystem accounting failed", "path", s.path, "err", err)
	}
	return s.decide(usage), nil
}

func filesystemCapacity(path string) (total, available int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return int64(stat.Blocks) * int64(stat.Bsize), int64(stat.Bavail) * int64(stat.Bsize), nil //nolint:gosec -- kernel values are bounded by the mounted filesystem
}

func capacityBounded(allocation, filesystemTotal int64) bool {
	if allocation <= 0 || filesystemTotal <= 0 {
		return false
	}
	tolerance := allocation / 20
	if tolerance < 64*1024*1024 {
		tolerance = 64 * 1024 * 1024
	}
	return filesystemTotal <= allocation+tolerance
}

// decide resolves a raw sample into a reserve verdict, applying hysteresis
// against the sampler's previous decision and recording the result.
func (s *diskSampler) decide(usage resourceUsage) resourceUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveReached = diskreserve.Decide(s.reserveReached, usage.DiskUsedBytes, usage.DiskTotalBytes, usage.DiskUsageState)
	usage.DiskReserveReached = s.reserveReached
	return usage
}

func (s *diskSampler) scan(startedAt time.Time) {
	used, partial, err := s.walk(s.path)
	s.mu.Lock()
	if err != nil {
		s.state = "error"
	} else {
		s.usedBytes = used
		s.sampledAt = startedAt
		s.state = "ready"
		if partial {
			s.state = "partial"
		}
	}
	s.mu.Unlock()
	s.running.Store(false)

	if err != nil && s.warn != nil {
		s.warn("disk directory accounting failed", "path", s.path, "err", err)
	} else if partial && s.warn != nil {
		s.warn("disk directory accounting skipped unreadable paths", "path", s.path, "usedBytes", used)
	}
}

func diskUsage(total, used int64, method, state string, sampledAt time.Time) resourceUsage {
	usage := resourceUsage{
		DiskUsedBytes:   used,
		DiskTotalBytes:  total,
		DiskUsageMethod: method,
		DiskUsageState:  state,
	}
	if !sampledAt.IsZero() {
		usage.DiskUsedSampledAt = sampledAt.UTC().Format(time.RFC3339)
	}
	// The reserve verdict is applied by diskSampler.decide, which has the
	// previous decision the hysteresis band needs.
	return usage
}

func mountRoot(mountInfoPath, target string) (string, error) {
	f, err := os.Open(mountInfoPath)
	if err != nil {
		return "", fmt.Errorf("open mountinfo: %w", err)
	}
	defer f.Close()
	target = filepath.Clean(target)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 5 && unescapeMountInfo(fields[4]) == target {
			return unescapeMountInfo(fields[3]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read mountinfo: %w", err)
	}
	return "", fmt.Errorf("mountpoint %s missing from mountinfo", target)
}

func unescapeMountInfo(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}

func directoryUsage(root string) (int64, bool, error) {
	seen := make(map[[2]uint64]struct{})
	var total int64
	var rootDevice uint64
	entries := 0
	partial := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrPermission) {
				partial = true
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				partial = true
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return err
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			device := uint64(stat.Dev)
			if path == root {
				rootDevice = device
			} else if rootDevice != 0 && device != rootDevice {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if stat.Nlink > 1 {
				key := [2]uint64{device, stat.Ino}
				if _, duplicate := seen[key]; duplicate {
					return nil
				}
				seen[key] = struct{}{}
			}
			total += stat.Blocks * 512 // allocated bytes, matching du rather than apparent size
		} else {
			total += info.Size()
		}
		entries++
		if entries%directoryScanYieldEvery == 0 {
			time.Sleep(time.Millisecond)
		}
		return nil
	})
	if err != nil {
		return 0, false, fmt.Errorf("walk %s: %w", root, err)
	}
	return total, partial, nil
}

func readInt64File(path string) (int64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return value, nil
}

func readOptionalInt64File(path string) (*int64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	value := strings.TrimSpace(string(body))
	if value == "max" {
		return nil, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &n, nil
}

func readCPUUsageUsec(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), " ")
		if key != "usage_usec" || !ok {
			continue
		}
		return strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return 0, fmt.Errorf("usage_usec missing from %s", path)
}

func readCPULimit(path string) (*int64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	fields := strings.Fields(string(body))
	if len(fields) != 2 {
		return nil, fmt.Errorf("parse %s: expected quota and period", path)
	}
	if fields[0] == "max" {
		return nil, nil
	}
	quota, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil, fmt.Errorf("parse quota %s: %w", path, err)
	}
	period, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return nil, fmt.Errorf("parse period %s: %w", path, err)
	}
	if period <= 0 {
		return nil, fmt.Errorf("parse period %s: must be positive", path)
	}
	limit := int64(quota / period * 1000)
	return &limit, nil
}
