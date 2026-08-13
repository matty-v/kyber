package adapters

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

func init() {
	RegisterComputeProvider("fake", func(_ context.Context, _ ProviderConfig) (ComputeAdapter, error) {
		return NewFakeComputeAdapter(), nil
	})
}

type fakeInstance struct {
	spec        MachineSpec
	observation InstanceObservation
}

// FakeComputeAdapter is a deterministic managed-provider simulator. Unlike
// the compatibility mock/static path, Machines using it traverse the normal
// Machine state machine and finalizer.
type FakeComputeAdapter struct {
	mu          sync.Mutex
	instances   map[string]*fakeInstance
	createError error
}

func NewFakeComputeAdapter() *FakeComputeAdapter {
	return &FakeComputeAdapter{instances: map[string]*fakeInstance{}}
}

func (f *FakeComputeAdapter) Type() string { return "fake" }

func (f *FakeComputeAdapter) NodeAttachment() NodeAttachmentMode { return NodeAttachmentExisting }

func (f *FakeComputeAdapter) CreateInstance(_ context.Context, spec MachineSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createError != nil {
		return "", f.createError
	}
	for id, instance := range f.instances {
		if instance.spec.Name == spec.Name {
			return id, nil
		}
	}
	id := "fake://instance/" + uuid.NewString()
	f.instances[id] = &fakeInstance{
		spec: spec,
		observation: InstanceObservation{
			State:        InstanceStateRunning,
			Interruption: InterruptionNone,
			Location:     spec.Zone,
			InternalIP:   "192.0.2.10",
			ExternalIP:   "198.51.100.10",
			CreatedAt:    time.Now().UTC(),
		},
	}
	return id, nil
}

func (f *FakeComputeAdapter) StartInstance(_ context.Context, instanceID string) error {
	return f.update(instanceID, func(observation *InstanceObservation) {
		observation.State = InstanceStateRunning
		observation.Interruption = InterruptionNone
	})
}

func (f *FakeComputeAdapter) StopInstance(_ context.Context, instanceID string) error {
	return f.update(instanceID, func(observation *InstanceObservation) {
		observation.State = InstanceStateStopped
	})
}

func (f *FakeComputeAdapter) DeleteInstance(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.instances, instanceID)
	return nil
}

func (f *FakeComputeAdapter) Observe(_ context.Context, instanceID string) (InstanceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	instance, ok := f.instances[instanceID]
	if !ok {
		return InstanceObservation{}, fmt.Errorf("fake instance %q not found", instanceID)
	}
	return instance.observation, nil
}

// SetObservation scripts the next state returned by Observe. It is intended
// for unit and envtest assertions, including interruption and failure paths.
func (f *FakeComputeAdapter) SetObservation(instanceID string, observation InstanceObservation) error {
	return f.update(instanceID, func(current *InstanceObservation) {
		*current = observation
	})
}

func (f *FakeComputeAdapter) SetCreateError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createError = err
}

func (f *FakeComputeAdapter) InstanceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.instances)
}

func (f *FakeComputeAdapter) update(instanceID string, update func(*InstanceObservation)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	instance, ok := f.instances[instanceID]
	if !ok {
		return fmt.Errorf("fake instance %q not found", instanceID)
	}
	update(&instance.observation)
	return nil
}

var _ ComputeAdapter = (*FakeComputeAdapter)(nil)
