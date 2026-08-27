package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const diskReserveRatio = 0.90

type resourceUsage struct {
	CPUUsageMillicores int64  `json:"cpuUsageMillicores"`
	CPULimitMillicores *int64 `json:"cpuLimitMillicores,omitempty"`
	MemoryUsedBytes    int64  `json:"memoryUsedBytes"`
	MemoryLimitBytes   *int64 `json:"memoryLimitBytes,omitempty"`
	DiskUsedBytes      int64  `json:"diskUsedBytes"`
	DiskTotalBytes     int64  `json:"diskTotalBytes"`
	DiskReserveReached bool   `json:"diskReserveReached"`
}

type resourceSampler struct {
	persistPath  string
	cgroupPath   string
	previousCPU  uint64
	previousTime time.Time
}

func newResourceSampler(persistPath, cgroupEventsPath string) *resourceSampler {
	cgroupPath := ""
	if cgroupEventsPath != "" {
		cgroupPath = strings.TrimSuffix(cgroupEventsPath, "/memory.events")
	}
	return &resourceSampler{persistPath: persistPath, cgroupPath: cgroupPath}
}

func (s *resourceSampler) sample(now time.Time) (resourceUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(s.persistPath, &stat); err != nil {
		return resourceUsage{}, fmt.Errorf("statfs %s: %w", s.persistPath, err)
	}
	total := int64(stat.Blocks) * int64(stat.Bsize) //nolint:gosec -- kernel values are bounded by the mounted filesystem
	free := int64(stat.Bavail) * int64(stat.Bsize)  //nolint:gosec -- see above
	usage := resourceUsage{
		DiskUsedBytes:  total - free,
		DiskTotalBytes: total,
	}
	if total > 0 {
		usage.DiskReserveReached = float64(usage.DiskUsedBytes)/float64(total) >= diskReserveRatio
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
