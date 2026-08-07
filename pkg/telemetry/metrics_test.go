package telemetry

import (
	"sync"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
)

func TestInitMetrics_Idempotent(t *testing.T) {
	// Reset global state so this test is self-contained.
	resetMeterOnce()

	// First call should succeed.
	if err := InitMetrics(); err != nil {
		t.Fatalf("InitMetrics() first call: %v", err)
	}

	// Second call must not panic and must return nil (same result as first).
	if err := InitMetrics(); err != nil {
		t.Fatalf("InitMetrics() second call: %v", err)
	}
}

func TestInitMetrics_AllCountersNonNil(t *testing.T) {
	resetMeterOnce()

	if err := InitMetrics(); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	if AgentStateTransitions == nil {
		t.Error("AgentStateTransitions should be non-nil after InitMetrics")
	}
	if AgentReconcileLatency == nil {
		t.Error("AgentReconcileLatency should be non-nil after InitMetrics")
	}
	if AgentReconcileErrors == nil {
		t.Error("AgentReconcileErrors should be non-nil after InitMetrics")
	}
	if AgentRestarts == nil {
		t.Error("AgentRestarts should be non-nil after InitMetrics")
	}
	if MachineStateTransitions == nil {
		t.Error("MachineStateTransitions should be non-nil after InitMetrics")
	}
	if MachineReconcileLatency == nil {
		t.Error("MachineReconcileLatency should be non-nil after InitMetrics")
	}
	if MachinePreemptions == nil {
		t.Error("MachinePreemptions should be non-nil after InitMetrics")
	}
}

func TestRegisterAll_WithNoop(t *testing.T) {
	m := noop.NewMeterProvider().Meter(meterName)
	if err := registerAll(m); err != nil {
		t.Fatalf("registerAll(noop): %v", err)
	}
}

func TestInitMetrics_ConcurrentSafe(t *testing.T) {
	resetMeterOnce()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Must not race or panic.
			_ = InitMetrics()
		}()
	}
	wg.Wait()
}

// resetMeterOnce clears the sync.Once so each test can call InitMetrics from scratch.
// Only valid within the package (white-box test). Do not call from production code.
func resetMeterOnce() {
	meterOnce = sync.Once{}
	initErr = nil
	AgentStateTransitions = nil
	AgentReconcileLatency = nil
	AgentReconcileErrors = nil
	AgentRestarts = nil
	MachineStateTransitions = nil
	MachineReconcileLatency = nil
	MachinePreemptions = nil
}
