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

func init() {
	RegisterComputeProvider("mock", func(_ context.Context, _ ProviderConfig) (ComputeAdapter, error) {
		return NewMockComputeAdapter(), nil
	})
	RegisterComputeProvider("static", func(_ context.Context, _ ProviderConfig) (ComputeAdapter, error) {
		return NewStaticComputeAdapter(), nil
	})
}

// mockInstance holds the in-memory state of a mock VM instance.
type mockInstance struct {
	observation InstanceObservation
	spec        MachineSpec
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

// Type returns the registered provider identifier.
func (m *MockComputeAdapter) Type() string { return "mock" }

// NodeAttachment reports that compatibility mock Machines use an existing node.
func (m *MockComputeAdapter) NodeAttachment() NodeAttachmentMode { return NodeAttachmentExisting }

// StaticComputeAdapter preserves the existing-node behavior under its accurate
// provider name. Its lifecycle methods are inherited for interface symmetry,
// but the static reconciler path does not call them.
type StaticComputeAdapter struct{ *MockComputeAdapter }

func NewStaticComputeAdapter() *StaticComputeAdapter {
	return &StaticComputeAdapter{MockComputeAdapter: NewMockComputeAdapter()}
}

func (s *StaticComputeAdapter) Type() string { return "static" }

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
		observation: InstanceObservation{
			State:        InstanceStateRunning,
			Interruption: InterruptionNone,
			Location:     spec.Location,
			InternalIP:   ip,
			ExternalIP:   ip, // mock uses same IP for both
			CreatedAt:    time.Now(),
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
	inst.observation.State = InstanceStateRunning
	inst.observation.Interruption = InterruptionNone
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
	inst.observation.State = InstanceStateStopped
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

// Observe returns the current provider-neutral observation of the mock instance.
// Returns an error if the instance is not found.
func (m *MockComputeAdapter) Observe(_ context.Context, instanceID string) (InstanceObservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[instanceID]
	if !ok {
		return InstanceObservation{}, fmt.Errorf("instance %q not found", instanceID)
	}
	return inst.observation, nil
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
	inst.observation.State = InstanceStateStopped
	inst.observation.Interruption = InterruptionPreempted
	return nil
}

// Instances returns a snapshot of all mock instances (keyed by instance ID).
// Intended for test assertions only.
func (m *MockComputeAdapter) Instances() map[string]InstanceObservation {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]InstanceObservation, len(m.instances))
	for id, inst := range m.instances {
		out[id] = inst.observation
	}
	return out
}

// InstanceCount returns the number of tracked mock instances.
func (m *MockComputeAdapter) InstanceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.instances)
}
