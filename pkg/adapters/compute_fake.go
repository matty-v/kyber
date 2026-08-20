package adapters

import (
	"context"
	"fmt"
	"net/url"
	"strings"
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

// Capabilities describes the operator actions supported by the deterministic
// managed-capacity simulator.
func (f *FakeComputeAdapter) Capabilities(_ context.Context) (Capabilities, error) {
	return Capabilities{
		CanProvision:          true,
		CanDiscoverExisting:   false,
		SuspendMode:           SuspendCapacity,
		DeletionMode:          DeleteCapacity,
		SupportsReliable:      true,
		SupportsInterruptible: true,
		SupportsLocations:     true,
	}, nil
}

// Profiles returns the provider-neutral profile used by contract tests. The
// active API continues serving its configured catalog until the profile API
// migration is complete.
func (f *FakeComputeAdapter) Profiles(_ context.Context) ([]Profile, error) {
	return []Profile{{
		ID:                  "e2-small",
		DisplayName:         "Small",
		CPU:                 "2",
		Memory:              "2Gi",
		AvailabilityClasses: []string{"reliable", "costOptimized"},
		Recommended:         true,
	}}, nil
}

// Validate checks only the portable intent understood by the fake provider.
func (f *FakeComputeAdapter) Validate(_ context.Context, desired DesiredMachine) error {
	switch desired.Availability {
	case DesiredOnline, DesiredOffline, DesiredDeleted:
		return nil
	default:
		return fmt.Errorf("validating fake capacity: unsupported desired availability %q", desired.Availability)
	}
}

// Reconcile converges deterministic fake capacity toward the requested
// availability. It implements the new provider contract alongside the legacy
// ComputeAdapter methods while the Machine reconciler is migrated.
func (f *FakeComputeAdapter) Reconcile(
	ctx context.Context,
	identity MachineIdentity,
	desired DesiredMachine,
	ref ProviderRef,
) (CapacityObservation, error) {
	if err := f.Validate(ctx, desired); err != nil {
		return CapacityObservation{}, err
	}

	selector := map[string]string{MachineLabelKey: identity.Name}
	switch desired.Availability {
	case DesiredDeleted:
		if ref != "" {
			if err := f.DeleteInstance(ctx, string(ref)); err != nil {
				return CapacityObservation{}, fmt.Errorf("deleting fake capacity: %w", err)
			}
		}
		return CapacityObservation{State: CapacityAbsent, Reason: ReasonDeleted}, nil

	case DesiredOffline:
		if ref == "" {
			return CapacityObservation{State: CapacityOffline, Reason: ReasonStopped}, nil
		}
		if err := f.StopInstance(ctx, string(ref)); err != nil {
			return CapacityObservation{}, fmt.Errorf("stopping fake capacity: %w", err)
		}
		observation, err := f.Observe(ctx, string(ref))
		if err != nil {
			return CapacityObservation{}, fmt.Errorf("observing stopped fake capacity: %w", err)
		}
		return CapacityObservationFromInstance(ref, selector, observation), nil

	case DesiredOnline:
		if ref == "" {
			return f.createCapacity(ctx, identity, desired, selector)
		}
		observation, err := f.Observe(ctx, string(ref))
		if err != nil {
			return CapacityObservation{}, fmt.Errorf("observing fake capacity: %w", err)
		}
		if observation.Interruption == InterruptionPreempted {
			if !desired.Interruptible {
				return CapacityObservation{
					State: CapacityFailed, Reason: ReasonProviderError,
					Message: "non-interruptible fake capacity was interrupted", ProviderRef: ref,
					NodeSelector: selector,
				}, nil
			}
			return f.createCapacity(ctx, identity, desired, selector)
		}
		if observation.State == InstanceStateStopped {
			if err := f.StartInstance(ctx, string(ref)); err != nil {
				return CapacityObservation{}, fmt.Errorf("starting fake capacity: %w", err)
			}
			observation, err = f.Observe(ctx, string(ref))
			if err != nil {
				return CapacityObservation{}, fmt.Errorf("observing started fake capacity: %w", err)
			}
		}
		return CapacityObservationFromInstance(ref, selector, observation), nil
	}

	return CapacityObservation{}, fmt.Errorf("reconciling fake capacity: unsupported desired availability %q", desired.Availability)
}

func (f *FakeComputeAdapter) createCapacity(
	ctx context.Context,
	identity MachineIdentity,
	desired DesiredMachine,
	selector map[string]string,
) (CapacityObservation, error) {
	id, err := f.CreateInstance(ctx, MachineSpec{
		Name:          identity.Name,
		Profile:       desired.Profile,
		DiskSizeGb:    desired.DiskSizeGb,
		Interruptible: desired.Interruptible,
		Location:      desired.Location,
		Labels:        desired.Labels,
		JoinToken:     desired.NodeBootstrap.JoinToken,
		ServerURL:     desired.NodeBootstrap.ServerURL,
	})
	if err != nil {
		return CapacityObservation{}, fmt.Errorf("creating fake capacity: %w", err)
	}
	observation, err := f.Observe(ctx, id)
	if err != nil {
		return CapacityObservation{}, fmt.Errorf("observing created fake capacity: %w", err)
	}
	return CapacityObservationFromInstance(ProviderRef(id), selector, observation), nil
}

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
	// Keep the machine name in the otherwise opaque ID so a replacement
	// control-plane process can reconstruct its in-memory simulator state from
	// the InstanceId persisted on the Machine CR.
	id := "fake://instance/" + url.PathEscape(spec.Name) + "/" + uuid.NewString()
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
	instance, err := f.instance(instanceID)
	if err != nil {
		return InstanceObservation{}, err
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
		// IDs created by the first fake-provider implementation did not encode
		// the Machine name. A restarted local stack can still adopt its sole
		// legacy instance when the scenario request supplies that name.
		if instance.spec.Name == "" && len(f.instances) == 1 {
			instance.spec.Name = machineName
		}
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
	instance, err := f.instance(instanceID)
	if err != nil {
		return err
	}
	update(&instance.observation)
	return nil
}

// instance returns an existing simulation record or reconstructs the default
// running observation encoded by a fake provider ID. Machine CR status keeps
// that ID across control-plane restarts, while this adapter is intentionally
// in-memory; lazy reconstruction makes the local stack restart-safe.
// f.mu must be held by the caller.
func (f *FakeComputeAdapter) instance(instanceID string) (*fakeInstance, error) {
	if instance, ok := f.instances[instanceID]; ok {
		return instance, nil
	}
	const prefix = "fake://instance/"
	remainder := strings.TrimPrefix(instanceID, prefix)
	parts := strings.SplitN(remainder, "/", 2)
	if remainder == instanceID || parts[0] == "" {
		return nil, fmt.Errorf("fake instance %q not found", instanceID)
	}
	machineName := ""
	if len(parts) == 2 {
		var err error
		machineName, err = url.PathUnescape(parts[0])
		if err != nil || machineName == "" || parts[1] == "" {
			return nil, fmt.Errorf("fake instance %q not found", instanceID)
		}
	}
	instance := &fakeInstance{
		spec: MachineSpec{Name: machineName},
		observation: InstanceObservation{
			State:        InstanceStateRunning,
			Interruption: InterruptionNone,
		},
	}
	f.instances[instanceID] = instance
	return instance, nil
}

var _ ComputeAdapter = (*FakeComputeAdapter)(nil)
var _ CapacityProvider = (*FakeComputeAdapter)(nil)
var _ SimulationController = (*FakeComputeAdapter)(nil)
