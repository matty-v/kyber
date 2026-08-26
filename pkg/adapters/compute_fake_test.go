package adapters

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFakeComputeAdapterLifecycle(t *testing.T) {
	ctx := context.Background()
	adapter := NewFakeComputeAdapter()
	id, err := adapter.CreateInstance(ctx, MachineSpec{Name: "local", Location: "local-a", Interruptible: true})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if !strings.HasPrefix(id, "fake://instance/") {
		t.Errorf("instance ID = %q, want opaque fake URI", id)
	}
	if adapter.InstanceCount() != 1 {
		t.Errorf("InstanceCount() = %d, want 1", adapter.InstanceCount())
	}

	if err := adapter.StopInstance(ctx, id); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	observation, err := adapter.Observe(ctx, id)
	if err != nil {
		t.Fatalf("Observe stopped: %v", err)
	}
	if observation.State != InstanceStateStopped {
		t.Errorf("stopped state = %q, want %q", observation.State, InstanceStateStopped)
	}

	if err := adapter.StartInstance(ctx, id); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	observation, err = adapter.Observe(ctx, id)
	if err != nil {
		t.Fatalf("Observe running: %v", err)
	}
	if observation.State != InstanceStateRunning {
		t.Errorf("running state = %q, want %q", observation.State, InstanceStateRunning)
	}

	if err := adapter.DeleteInstance(ctx, id); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if err := adapter.DeleteInstance(ctx, id); err != nil {
		t.Fatalf("idempotent DeleteInstance: %v", err)
	}
}

func TestFakeComputeAdapterScriptedObservation(t *testing.T) {
	ctx := context.Background()
	adapter := NewFakeComputeAdapter()
	id, err := adapter.CreateInstance(ctx, MachineSpec{Name: "spot"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	want := InstanceObservation{
		State:        InstanceStateStopped,
		Interruption: InterruptionPreempted,
		Location:     "local-a",
	}
	if err := adapter.SetObservation(id, want); err != nil {
		t.Fatalf("SetObservation: %v", err)
	}
	got, err := adapter.Observe(ctx, id)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got != want {
		t.Errorf("Observe() = %#v, want %#v", got, want)
	}
}

func TestFakeComputeAdapterCreateFailure(t *testing.T) {
	adapter := NewFakeComputeAdapter()
	want := errors.New("quota exhausted")
	adapter.SetCreateError(want)
	_, err := adapter.CreateInstance(context.Background(), MachineSpec{Name: "failure"})
	if !errors.Is(err, want) {
		t.Errorf("CreateInstance error = %v, want %v", err, want)
	}
}

func TestFakeComputeAdapterReplacesPreemptedInstance(t *testing.T) {
	adapter := NewFakeComputeAdapter()
	ctx := context.Background()
	spec := MachineSpec{Name: "worker", Location: "local-a", Interruptible: true}
	firstID, err := adapter.CreateInstance(ctx, spec)
	if err != nil {
		t.Fatalf("first CreateInstance: %v", err)
	}
	if err := adapter.ApplySimulationScenario("worker", SimulationPreempted); err != nil {
		t.Fatalf("preempt: %v", err)
	}
	secondID, err := adapter.CreateInstance(ctx, spec)
	if err != nil {
		t.Fatalf("replacement CreateInstance: %v", err)
	}
	if secondID == firstID {
		t.Fatalf("replacement reused preempted ID %q", firstID)
	}
	observation, err := adapter.Observe(ctx, secondID)
	if err != nil {
		t.Fatalf("Observe replacement: %v", err)
	}
	if observation.State != InstanceStateRunning || observation.Interruption != InterruptionNone {
		t.Fatalf("replacement observation = %+v", observation)
	}
}

func TestFakeComputeAdapterRecoversPersistedInstanceAfterRestart(t *testing.T) {
	ctx := context.Background()
	beforeRestart := NewFakeComputeAdapter()
	id, err := beforeRestart.CreateInstance(ctx, MachineSpec{Name: "restart-worker", Location: "local-a"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	afterRestart := NewFakeComputeAdapter()
	observation, err := afterRestart.Observe(ctx, id)
	if err != nil {
		t.Fatalf("Observe recovered instance: %v", err)
	}
	if observation.State != InstanceStateRunning {
		t.Fatalf("recovered state = %q, want %q", observation.State, InstanceStateRunning)
	}
	if err := afterRestart.ApplySimulationScenario("restart-worker", SimulationFailed); err != nil {
		t.Fatalf("ApplySimulationScenario recovered instance: %v", err)
	}
	observation, err = afterRestart.Observe(ctx, id)
	if err != nil {
		t.Fatalf("Observe failed scenario: %v", err)
	}
	if observation.State != InstanceStateFailed {
		t.Fatalf("scenario state = %q, want %q", observation.State, InstanceStateFailed)
	}
}

func TestFakeComputeAdapterRecoversLegacyPersistedInstanceAfterRestart(t *testing.T) {
	adapter := NewFakeComputeAdapter()
	const legacyID = "fake://instance/45ce8ed8-c8ab-4c30-928f-6a9532aacd29"
	if _, err := adapter.Observe(context.Background(), legacyID); err != nil {
		t.Fatalf("Observe legacy instance: %v", err)
	}
	if err := adapter.ApplySimulationScenario("legacy-worker", SimulationPreempted); err != nil {
		t.Fatalf("ApplySimulationScenario legacy instance: %v", err)
	}
	observation, err := adapter.Observe(context.Background(), legacyID)
	if err != nil {
		t.Fatalf("Observe legacy scenario: %v", err)
	}
	if observation.Interruption != InterruptionPreempted {
		t.Fatalf("legacy interruption = %q, want %q", observation.Interruption, InterruptionPreempted)
	}
}

func TestFakeCapacityProviderLifecycle(t *testing.T) {
	ctx := context.Background()
	provider := NewFakeComputeAdapter()
	identity := MachineIdentity{Name: "capacity-worker"}
	desired := DesiredMachine{
		Availability:  DesiredOnline,
		Profile:       "default",
		Interruptible: true,
		Location:      "local-a",
		NodeBootstrap: NodeBootstrap{
			ServerURL: "https://cluster.example:6443",
			JoinToken: "test-only-token",
		},
	}

	created, err := provider.Reconcile(ctx, identity, desired, "")
	if err != nil {
		t.Fatalf("Reconcile Online create: %v", err)
	}
	if created.State != CapacityAvailable || created.Reason != ReasonReady {
		t.Fatalf("created observation = %+v, want Available/Ready", created)
	}
	if created.ProviderRef == "" {
		t.Fatal("created ProviderRef is empty")
	}
	if created.NodeSelector[MachineLabelKey] != identity.Name {
		t.Errorf("created NodeSelector = %#v, want machine label %q", created.NodeSelector, identity.Name)
	}
	instances := provider.ListSimulatedInstances()
	if len(instances) != 1 {
		t.Fatalf("ListSimulatedInstances() returned %d instances, want 1", len(instances))
	}
	if instances[0].Spec.ServerURL != desired.NodeBootstrap.ServerURL ||
		instances[0].Spec.JoinToken != desired.NodeBootstrap.JoinToken {
		t.Errorf("fake bootstrap = %#v, want desired bootstrap", instances[0].Spec)
	}

	desired.Availability = DesiredOffline
	stopped, err := provider.Reconcile(ctx, identity, desired, created.ProviderRef)
	if err != nil {
		t.Fatalf("Reconcile Offline: %v", err)
	}
	if stopped.State != CapacityOffline || stopped.Reason != ReasonStopped {
		t.Fatalf("stopped observation = %+v, want Offline/Stopped", stopped)
	}

	desired.Availability = DesiredOnline
	started, err := provider.Reconcile(ctx, identity, desired, created.ProviderRef)
	if err != nil {
		t.Fatalf("Reconcile Online start: %v", err)
	}
	if started.State != CapacityAvailable || started.ProviderRef != created.ProviderRef {
		t.Fatalf("started observation = %+v, want Available with original ref %q", started, created.ProviderRef)
	}

	desired.Availability = DesiredDeleted
	deleted, err := provider.Reconcile(ctx, identity, desired, created.ProviderRef)
	if err != nil {
		t.Fatalf("Reconcile Deleted: %v", err)
	}
	if deleted.State != CapacityAbsent || deleted.Reason != ReasonDeleted {
		t.Fatalf("deleted observation = %+v, want Absent/Deleted", deleted)
	}
	if provider.InstanceCount() != 0 {
		t.Errorf("InstanceCount() = %d, want 0", provider.InstanceCount())
	}
}

func TestFakeCapacityProviderOwnsInterruptionReplacement(t *testing.T) {
	ctx := context.Background()
	provider := NewFakeComputeAdapter()
	identity := MachineIdentity{Name: "spot-capacity"}
	desired := DesiredMachine{
		Availability:  DesiredOnline,
		Profile:       "default",
		Interruptible: true,
		Location:      "local-a",
	}

	first, err := provider.Reconcile(ctx, identity, desired, "")
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if err := provider.ApplySimulationScenario(identity.Name, SimulationPreempted); err != nil {
		t.Fatalf("ApplySimulationScenario: %v", err)
	}

	replacement, err := provider.Reconcile(ctx, identity, desired, first.ProviderRef)
	if err != nil {
		t.Fatalf("replacement Reconcile: %v", err)
	}
	if replacement.ProviderRef == first.ProviderRef {
		t.Fatalf("replacement ProviderRef = %q, want a new ref", replacement.ProviderRef)
	}
	if replacement.State != CapacityAvailable {
		t.Errorf("replacement State = %q, want %q", replacement.State, CapacityAvailable)
	}
	if provider.InstanceCount() != 1 {
		t.Errorf("InstanceCount() = %d, want 1", provider.InstanceCount())
	}
}

func TestFakeCapacityProviderCapabilities(t *testing.T) {
	provider := NewFakeComputeAdapter()
	got, err := provider.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !got.CanProvision || got.SuspendMode != SuspendCapacity || got.DeletionMode != DeleteCapacity {
		t.Errorf("Capabilities() = %+v, want managed lifecycle", got)
	}
	if !got.SupportsReliable || !got.SupportsInterruptible || !got.SupportsLocations {
		t.Errorf("Capabilities() = %+v, want all fake offering capabilities", got)
	}
	if got.ReliableFallbackMode != ReliableFallbackAutomatic {
		t.Errorf("ReliableFallbackMode = %q, want %q", got.ReliableFallbackMode, ReliableFallbackAutomatic)
	}
}

func TestFakeCapacityProviderFallbackRetainsProviderRefAndRetriesCostOptimized(t *testing.T) {
	ctx := context.Background()
	provider := NewFakeComputeAdapter()
	identity := MachineIdentity{Name: "fallback-worker"}
	desired := DesiredMachine{Availability: DesiredOnline, AvailabilityClass: "costOptimized", Interruptible: true, Location: "local-a"}

	created, err := provider.Reconcile(ctx, identity, desired, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := provider.ApplySimulationScenario(identity.Name, SimulationCostOptimizedUnavailable); err != nil {
		t.Fatalf("fallback scenario: %v", err)
	}
	fallback, err := provider.Reconcile(ctx, identity, desired, created.ProviderRef)
	if err != nil {
		t.Fatalf("fallback reconcile: %v", err)
	}
	if fallback.ProviderRef != created.ProviderRef {
		t.Fatalf("fallback ProviderRef = %q, want retained %q", fallback.ProviderRef, created.ProviderRef)
	}
	if fallback.State != CapacityAvailable || fallback.EffectiveAvailabilityClass != "reliable" {
		t.Fatalf("fallback observation = %+v, want Available/reliable", fallback)
	}
	if fallback.FallbackSince.IsZero() || fallback.CostOptimizedUnavailableSince.IsZero() {
		t.Fatalf("fallback timestamps not populated: %+v", fallback)
	}

	desired.CostOptimizedRetryRequest = "retry-1"
	retried, err := provider.Reconcile(ctx, identity, desired, created.ProviderRef)
	if err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	if retried.ProviderRef != created.ProviderRef || retried.EffectiveAvailabilityClass != "costOptimized" {
		t.Fatalf("retry observation = %+v, want same ref and costOptimized", retried)
	}
	if retried.CostOptimizedRetryObserved != "retry-1" || !retried.FallbackSince.IsZero() {
		t.Fatalf("retry acknowledgement = %+v", retried)
	}
}

func TestFakeCapacityProviderFailedRetryRollsBackToReliable(t *testing.T) {
	ctx := context.Background()
	provider := NewFakeComputeAdapter()
	identity := MachineIdentity{Name: "rollback-worker"}
	desired := DesiredMachine{Availability: DesiredOnline, AvailabilityClass: "costOptimized", Interruptible: true, Location: "local-a"}
	created, err := provider.Reconcile(ctx, identity, desired, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := provider.ApplySimulationScenario(identity.Name, SimulationCostOptimizedUnavailable); err != nil {
		t.Fatalf("fallback scenario: %v", err)
	}
	if err := provider.ApplySimulationScenario(identity.Name, SimulationFailNextCostOptimizedRetry); err != nil {
		t.Fatalf("fail retry scenario: %v", err)
	}
	desired.CostOptimizedRetryRequest = "retry-rollback"
	got, err := provider.Reconcile(ctx, identity, desired, created.ProviderRef)
	if err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	if got.ProviderRef != created.ProviderRef || got.EffectiveAvailabilityClass != "reliable" {
		t.Fatalf("rollback observation = %+v, want same ref and reliable", got)
	}
	if got.CostOptimizedRetryObserved != "retry-rollback" || !strings.Contains(got.FallbackReason, "retained") {
		t.Fatalf("rollback acknowledgement = %+v", got)
	}
}
