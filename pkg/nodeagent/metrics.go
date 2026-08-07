// Package nodeagent implements the Kyber node agent — a thin DaemonSet binary that
// collects host-level metrics and executes machine-level actions (reboot, stop).
package nodeagent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Metrics holds a single point-in-time snapshot of host metrics.
type Metrics struct {
	Timestamp    time.Time
	CPUPercent   float64 // percentage over the collection interval
	MemoryUsed   uint64  // bytes
	MemoryTotal  uint64  // bytes
	DiskUsed     uint64  // bytes (root filesystem)
	DiskTotal    uint64  // bytes
	UptimeSecs   int64
	Load1        float64
	Load5        float64
	Load15       float64
}

// CPUSnapshot holds raw /proc/stat counters for delta calculation.
type CPUSnapshot struct {
	User    uint64
	Nice    uint64
	System  uint64
	Idle    uint64
	Iowait  uint64
	Irq     uint64
	Softirq uint64
	Steal   uint64
}

// total returns the sum of all CPU time fields.
func (s *CPUSnapshot) total() uint64 {
	return s.User + s.Nice + s.System + s.Idle + s.Iowait + s.Irq + s.Softirq + s.Steal
}

// idle returns idle + iowait (time CPU was not doing useful work).
func (s *CPUSnapshot) idle() uint64 {
	return s.Idle + s.Iowait
}

// Collector reads host metrics from /proc (or a fake path for testing).
type Collector struct {
	procPath string
	prev     *CPUSnapshot
}

// NewCollector creates a Collector that reads from procPath.
// Pass "/proc" for production; pass a test fixture directory for unit tests.
func NewCollector(procPath string) *Collector {
	return &Collector{procPath: procPath}
}

// Collect reads all metrics from /proc. On the first call, CPUPercent is 0.0
// because there is no prior snapshot to diff against; subsequent calls compute
// CPU usage over the interval since the last call.
func (c *Collector) Collect() (*Metrics, error) {
	m := &Metrics{Timestamp: time.Now()}

	// CPU.
	cpu, err := c.parseCPUStat()
	if err != nil {
		return nil, fmt.Errorf("parsing /proc/stat: %w", err)
	}
	if c.prev != nil {
		totalDelta := cpu.total() - c.prev.total()
		idleDelta := cpu.idle() - c.prev.idle()
		if totalDelta > 0 {
			m.CPUPercent = (1.0 - float64(idleDelta)/float64(totalDelta)) * 100.0
		}
	}
	c.prev = cpu

	// Memory.
	memTotal, memAvail, err := c.parseMeminfo()
	if err != nil {
		return nil, fmt.Errorf("parsing /proc/meminfo: %w", err)
	}
	m.MemoryTotal = memTotal
	m.MemoryUsed = memTotal - memAvail

	// Uptime.
	uptime, err := c.parseUptime()
	if err != nil {
		return nil, fmt.Errorf("parsing /proc/uptime: %w", err)
	}
	m.UptimeSecs = uptime

	// Load averages.
	load1, load5, load15, err := c.parseLoadavg()
	if err != nil {
		return nil, fmt.Errorf("parsing /proc/loadavg: %w", err)
	}
	m.Load1 = load1
	m.Load5 = load5
	m.Load15 = load15

	// Disk — use syscall.Statfs on the root filesystem.
	diskUsed, diskTotal, err := getDiskStats("/")
	if err != nil {
		return nil, fmt.Errorf("getting disk stats: %w", err)
	}
	m.DiskUsed = diskUsed
	m.DiskTotal = diskTotal

	return m, nil
}

// parseCPUStat reads the first "cpu" line from /proc/stat.
func (c *Collector) parseCPUStat() (*CPUSnapshot, error) {
	f, err := os.Open(filepath.Join(c.procPath, "stat"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		// fields[0] = "cpu", [1]=user [2]=nice [3]=system [4]=idle [5]=iowait [6]=irq [7]=softirq [8]=steal
		if len(fields) < 8 {
			return nil, fmt.Errorf("unexpected /proc/stat cpu line: %q", line)
		}
		snap := &CPUSnapshot{}
		vals := []*uint64{&snap.User, &snap.Nice, &snap.System, &snap.Idle, &snap.Iowait, &snap.Irq, &snap.Softirq, &snap.Steal}
		for i, vp := range vals {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parsing cpu field %d: %w", i+1, err)
			}
			*vp = v
		}
		return snap, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no cpu line found in /proc/stat")
}

// parseMeminfo parses /proc/meminfo and returns (MemTotal, MemAvailable) in bytes.
// Falls back to MemFree if MemAvailable is not present (older kernels).
func (c *Collector) parseMeminfo() (total, available uint64, err error) {
	f, err := os.Open(filepath.Join(c.procPath, "meminfo"))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	var memFree uint64
	var hasAvail bool

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		// Values in /proc/meminfo are in kB.
		switch key {
		case "MemTotal":
			total = val * 1024
		case "MemFree":
			memFree = val * 1024
		case "MemAvailable":
			available = val * 1024
			hasAvail = true
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
	}
	if !hasAvail {
		available = memFree
	}
	return total, available, nil
}

// parseUptime reads /proc/uptime and returns integer seconds since boot.
func (c *Collector) parseUptime() (int64, error) {
	f, err := os.Open(filepath.Join(c.procPath, "uptime"))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 1 {
			return 0, fmt.Errorf("unexpected /proc/uptime format")
		}
		secs, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0, fmt.Errorf("parsing uptime: %w", err)
		}
		return int64(secs), nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("empty /proc/uptime")
}

// parseLoadavg reads /proc/loadavg and returns the 1, 5, and 15 minute load averages.
func (c *Collector) parseLoadavg() (load1, load5, load15 float64, err error) {
	f, err := os.Open(filepath.Join(c.procPath, "loadavg"))
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			return 0, 0, 0, fmt.Errorf("unexpected /proc/loadavg format: %q", scanner.Text())
		}
		parse := func(s string) (float64, error) {
			return strconv.ParseFloat(s, 64)
		}
		if load1, err = parse(fields[0]); err != nil {
			return 0, 0, 0, fmt.Errorf("parsing load1: %w", err)
		}
		if load5, err = parse(fields[1]); err != nil {
			return 0, 0, 0, fmt.Errorf("parsing load5: %w", err)
		}
		if load15, err = parse(fields[2]); err != nil {
			return 0, 0, 0, fmt.Errorf("parsing load15: %w", err)
		}
		return load1, load5, load15, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, err
	}
	return 0, 0, 0, fmt.Errorf("empty /proc/loadavg")
}
