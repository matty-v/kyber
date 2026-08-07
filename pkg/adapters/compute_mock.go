package adapters

// MockComputeAdapter is an in-memory, synchronous implementation of ComputeAdapter.
// It is intended for use in:
//   - Unit and integration tests (deterministic, no network calls)
//   - Local k3d development environments
//
// Do NOT use in production. The GCEAdapter is the production implementation.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// mockInstance holds the in-memory state of a mock VM instance.
type mockInstance struct {
	status     InstanceStatus
	preempted  bool
	spec       MachineSpec
}

// MockComputeAdapter implements ComputeAdapter using an in-memory map.
type MockComputeAdapter struct {
	mu          sync.Mutex
	instances   map[string]*mockInstance
	ipCounter   int
	createError error // if non-nil, CreateInstance returns this error
}

// NewMockComputeAdapter returns a ready-to-use MockComputeAdapter.
func NewMockComputeAdapter() *MockComputeAdapter {
	return &MockComputeAdapter{
		instances: make(map[string]*mockInstance),
		ipCounter: 1,
	}
}

// SetCreateError configures CreateInstance to return the given error on every call.
// Pass nil to clear the error and restore normal behavior.
// This is a test-only method for simulating CreateInstance failures.
func (m *MockComputeAdapter) SetCreateError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createError = err
}

// CreateInstance stores a new mock instance and returns a generated instance ID.
// The ID format is "mock-<uuid>". IPs are assigned as 10.0.0.N (incrementing).
// If SetCreateError was called with a non-nil error, that error is returned instead.
func (m *MockComputeAdapter) CreateInstance(_ context.Context, spec MachineSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.createError != nil {
		return "", m.createError
	}

	id := "mock-" + uuid.NewString()
	ip := fmt.Sprintf("10.0.0.%d", m.ipCounter)
	m.ipCounter++

	m.instances[id] = &mockInstance{
		spec: spec,
		status: InstanceStatus{
			Status:     InstanceStatusRunning,
			Zone:       spec.Zone,
			InternalIP: ip,
			ExternalIP: ip, // mock uses same IP for both
			CreatedAt:  time.Now(),
		},
	}
	return id, nil
}

// StartInstance transitions a stopped mock instance back to RUNNING.
func (m *MockComputeAdapter) StartInstance(_ context.Context, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[instanceID]
	if !ok {
		return fmt.Errorf("instance %q not found", instanceID)
	}
	inst.status.Status = InstanceStatusRunning
	inst.preempted = false
	return nil
}

// StopInstance transitions a mock instance to STOPPED.
func (m *MockComputeAdapter) StopInstance(_ context.Context, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[instanceID]
	if !ok {
		return fmt.Errorf("instance %q not found", instanceID)
	}
	inst.status.Status = InstanceStatusStopped
	return nil
}

// DeleteInstance removes the mock instance from the in-memory store.
func (m *MockComputeAdapter) DeleteInstance(_ context.Context, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.instances[instanceID]; !ok {
		return fmt.Errorf("instance %q not found", instanceID)
	}
	delete(m.instances, instanceID)
	return nil
}

// GetInstanceStatus returns the current status of the mock instance.
// Returns an error if the instance is not found.
func (m *MockComputeAdapter) GetInstanceStatus(_ context.Context, instanceID string) (InstanceStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[instanceID]
	if !ok {
		return InstanceStatus{}, fmt.Errorf("instance %q not found", instanceID)
	}
	return inst.status, nil
}

// IsPreempted returns whether the mock instance has been marked as preempted.
// In tests, call SetPreempted to simulate a spot preemption.
func (m *MockComputeAdapter) IsPreempted(_ context.Context, instanceID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[instanceID]
	if !ok {
		return false, fmt.Errorf("instance %q not found", instanceID)
	}
	return inst.preempted, nil
}

// SetPreempted marks an instance as preempted and transitions it to TERMINATED.
// This is a test-only method for simulating spot preemptions.
func (m *MockComputeAdapter) SetPreempted(instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[instanceID]
	if !ok {
		return fmt.Errorf("instance %q not found", instanceID)
	}
	inst.preempted = true
	inst.status.Status = InstanceStatusTerminated
	return nil
}

// Instances returns a snapshot of all mock instances (keyed by instance ID).
// Intended for test assertions only.
func (m *MockComputeAdapter) Instances() map[string]InstanceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]InstanceStatus, len(m.instances))
	for id, inst := range m.instances {
		out[id] = inst.status
	}
	return out
}

// InstanceCount returns the number of tracked mock instances.
func (m *MockComputeAdapter) InstanceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.instances)
}
