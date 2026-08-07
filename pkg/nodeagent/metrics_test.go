package nodeagent

import (
	"path/filepath"
	"runtime"
	"testing"
)

// fixtureDir returns the absolute path to a testdata fixture directory.
func fixtureDir(name string) string {
	_, callerFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(callerFile), "testdata", name)
}

// fixture1 is the base snapshot: cpu line "cpu 10000 500 3000 80000 1000 200 100 0 ..."
// total = 10000+500+3000+80000+1000+200+100 = 94800, idle = 80000+1000 = 81000.
//
// fixture2 is the follow-on snapshot: cpu line "cpu 11000 550 3300 80100 1050 220 110 0 ..."
// total = 11000+550+3300+80100+1050+220+110 = 96330, idle = 80100+1050 = 81150.
//
// delta total = 96330-94800 = 1530, delta idle = 81150-81000 = 150.
// CPU% = (1 - 150/1530) * 100 ≈ 90.196...%

func TestParseCPUStat_FirstCollect(t *testing.T) {
	c := NewCollector(fixtureDir("proc_fixture_1"))
	snap, err := c.parseCPUStat()
	if err != nil {
		t.Fatalf("parseCPUStat: %v", err)
	}
	if snap.User != 10000 {
		t.Errorf("User: want 10000, got %d", snap.User)
	}
	if snap.Nice != 500 {
		t.Errorf("Nice: want 500, got %d", snap.Nice)
	}
	if snap.System != 3000 {
		t.Errorf("System: want 3000, got %d", snap.System)
	}
	if snap.Idle != 80000 {
		t.Errorf("Idle: want 80000, got %d", snap.Idle)
	}
	if snap.Iowait != 1000 {
		t.Errorf("Iowait: want 1000, got %d", snap.Iowait)
	}
}

func TestCollect_CPUPercentZeroOnFirstCall(t *testing.T) {
	c := NewCollector(fixtureDir("proc_fixture_1"))
	m, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if m.CPUPercent != 0.0 {
		t.Errorf("CPUPercent on first call: want 0.0, got %f", m.CPUPercent)
	}
}

func TestCollect_CPUPercentDelta(t *testing.T) {
	// First collect from fixture_1 to establish the baseline snapshot.
	c := NewCollector(fixtureDir("proc_fixture_1"))
	if _, err := c.Collect(); err != nil {
		t.Fatalf("first Collect: %v", err)
	}

	// Manually parse fixture_2's CPU line and set it as the prev snapshot.
	// This simulates the passage of time: fixture_1 → fixture_2.
	c2 := &Collector{procPath: fixtureDir("proc_fixture_2"), prev: c.prev}
	m2, err := c2.Collect()
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}

	// delta total = 1530, delta idle = 150 → CPU% ≈ 90.196
	const wantApprox = 90.196
	const tolerance = 0.01
	if m2.CPUPercent < wantApprox-tolerance || m2.CPUPercent > wantApprox+tolerance {
		t.Errorf("CPUPercent: want ~%.3f, got %.3f", wantApprox, m2.CPUPercent)
	}

	// Sanity: idle delta != total delta (there was CPU work).
	snap1 := c.prev
	snap2, _ := c2.parseCPUStat() //nolint:errcheck — already passed above
	idleDelta := snap2.idle() - snap1.idle()
	totalDelta := snap2.total() - snap1.total()
	if idleDelta == totalDelta {
		t.Error("idle delta == total delta: no CPU work detected, test fixture may be wrong")
	}
}

func TestCollect_MemoryParsing(t *testing.T) {
	c := NewCollector(fixtureDir("proc_fixture_1"))
	m, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// meminfo: MemTotal = 16384000 kB = 16777216000000 bytes? No:
	// 16384000 kB * 1024 = 16777216000 bytes (≈ 16 GB)
	wantTotal := uint64(16384000) * 1024
	if m.MemoryTotal != wantTotal {
		t.Errorf("MemoryTotal: want %d, got %d", wantTotal, m.MemoryTotal)
	}

	// MemAvailable = 6144000 kB → MemUsed = MemTotal - MemAvailable
	wantAvail := uint64(6144000) * 1024
	wantUsed := wantTotal - wantAvail
	if m.MemoryUsed != wantUsed {
		t.Errorf("MemoryUsed: want %d, got %d", wantUsed, m.MemoryUsed)
	}
}

func TestCollect_UptimeParsing(t *testing.T) {
	c := NewCollector(fixtureDir("proc_fixture_1"))
	m, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// uptime file: "86400.12 ..." → int64(86400)
	if m.UptimeSecs != 86400 {
		t.Errorf("UptimeSecs: want 86400, got %d", m.UptimeSecs)
	}
}

func TestCollect_LoadavgParsing(t *testing.T) {
	c := NewCollector(fixtureDir("proc_fixture_1"))
	m, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// loadavg file: "1.25 0.75 0.50 ..."
	if m.Load1 != 1.25 {
		t.Errorf("Load1: want 1.25, got %f", m.Load1)
	}
	if m.Load5 != 0.75 {
		t.Errorf("Load5: want 0.75, got %f", m.Load5)
	}
	if m.Load15 != 0.50 {
		t.Errorf("Load15: want 0.50, got %f", m.Load15)
	}
}

func TestCollect_MissingFile(t *testing.T) {
	c := NewCollector("/nonexistent/proc/path/that/does/not/exist")
	_, err := c.Collect()
	if err == nil {
		t.Error("expected error for missing /proc path, got nil")
	}
}

func TestCPUSnapshot_IdleNeTotal(t *testing.T) {
	snap := &CPUSnapshot{
		User:    5000,
		Nice:    100,
		System:  2000,
		Idle:    80000,
		Iowait:  500,
		Irq:     50,
		Softirq: 25,
		Steal:   0,
	}
	idleTime := snap.idle()
	totalTime := snap.total()
	if idleTime == totalTime {
		t.Errorf("idle (%d) == total (%d): CPU snapshot has no active time", idleTime, totalTime)
	}
	if idleTime >= totalTime {
		t.Errorf("idle (%d) >= total (%d): impossible", idleTime, totalTime)
	}
}
