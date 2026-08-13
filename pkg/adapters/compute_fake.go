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
	mu         sync.Mutex
	instances  map[string]*fakeInstance
	nextErrors map[string]error
}

func NewFakeComputeAdapter() *FakeComputeAdapter {
	return &FakeComputeAdapter{instances: map[string]*fakeInstance{}, nextErrors: map[string]error{}}
}

func (f *FakeComputeAdapter) Type() string { return "fake" }

func (f *FakeComputeAdapter) NodeAttachment() NodeAttachmentMode { return NodeAttachmentExisting }

func (f *FakeComputeAdapter) CreateInstance(_ context.Context, spec MachineSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeError("create"); err != nil {
		return "", err
	}
	for id, instance := range f.instances {
		if instance.spec.Name == spec.Name {
			if instance.observation.Interruption == InterruptionPreempted {
				delete(f.instances, id)
				break
			}
			return id, nil
		}
	}
	id := "fake://instance/" + uuid.NewString()
	f.instances[id] = &fakeInstance{
		spec: spec,
		observation: InstanceObservation{
			State:        InstanceStateRunning,
			Interruption: InterruptionNone,
			Location:     spec.Location,
			InternalIP:   "192.0.2.10",
			ExternalIP:   "198.51.100.10",
			CreatedAt:    time.Now().UTC(),
		},
	}
	return id, nil
}

func (f *FakeComputeAdapter) StartInstance(_ context.Context, instanceID string) error {
	f.mu.Lock()
	if err := f.consumeError("start"); err != nil {
		f.mu.Unlock()
		return err
	}
	f.mu.Unlock()
	return f.update(instanceID, func(observation *InstanceObservation) {
		observation.State = InstanceStateRunning
		observation.Interruption = InterruptionNone
	})
}

func (f *FakeComputeAdapter) StopInstance(_ context.Context, instanceID string) error {
	f.mu.Lock()
	if err := f.consumeError("stop"); err != nil {
		f.mu.Unlock()
		return err
	}
	f.mu.Unlock()
	return f.update(instanceID, func(observation *InstanceObservation) {
		observation.State = InstanceStateStopped
	})
}

func (f *FakeComputeAdapter) DeleteInstance(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeError("delete"); err != nil {
		return err
	}
	delete(f.instances, instanceID)
	return nil
}

func (f *FakeComputeAdapter) Observe(_ context.Context, instanceID string) (InstanceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.consumeError("observe"); err != nil {
		return InstanceObservation{}, err
	}
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
	f.nextErrors["create"] = err
}

func (f *FakeComputeAdapter) ListSimulatedInstances() []SimulatedInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SimulatedInstance, 0, len(f.instances))
	for id, instance := range f.instances {
		out = append(out, SimulatedInstance{MachineName: instance.spec.Name, ProviderID: id, Spec: instance.spec, Observation: instance.observation})
	}
	return out
}

func (f *FakeComputeAdapter) ApplySimulationScenario(machineName string, scenario SimulationScenario) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	operation := map[SimulationScenario]string{
		SimulationFailNextCreate: "create", SimulationFailNextStart: "start",
		SimulationFailNextStop: "stop", SimulationFailNextDelete: "delete",
		SimulationFailNextObserve: "observe",
	}[scenario]
	if operation != "" {
		f.nextErrors[operation] = fmt.Errorf("simulated %s failure", operation)
		return nil
	}
	for _, instance := range f.instances {
		if instance.spec.Name != machineName {
			continue
		}
		instance.observation.Interruption = InterruptionNone
		switch scenario {
		case SimulationPending:
			instance.observation.State = InstanceStatePending
		case SimulationRunning:
			instance.observation.State = InstanceStateRunning
		case SimulationStopped:
			instance.observation.State = InstanceStateStopped
		case SimulationPreempted:
			instance.observation.State = InstanceStateStopped
			instance.observation.Interruption = InterruptionPreempted
		case SimulationFailed:
			instance.observation.State = InstanceStateFailed
		default:
			return fmt.Errorf("unknown simulation scenario %q", scenario)
		}
		return nil
	}
	return fmt.Errorf("fake machine %q not found", machineName)
}

// consumeError requires f.mu to be held.
func (f *FakeComputeAdapter) consumeError(operation string) error {
	err := f.nextErrors[operation]
	delete(f.nextErrors, operation)
	return err
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
var _ SimulationController = (*FakeComputeAdapter)(nil)
