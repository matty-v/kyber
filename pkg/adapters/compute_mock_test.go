package adapters

import (
	"context"
	"strings"
	"testing"
)

// TestMockComputeAdapter_CreateAndGet verifies that CreateInstance stores an instance
// and Observe returns it with provider-neutral Running state.
func TestMockComputeAdapter_CreateAndGet(t *testing.T) {
	m := NewMockComputeAdapter()
	ctx := context.Background()

	spec := MachineSpec{
		Name:          "test-machine",
		Profile:       "n2-standard-4",
		DiskSizeGb:    50,
		Interruptible: true,
		Location:      "us-central1-a",
	}

	id, err := m.CreateInstance(ctx, spec)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if !strings.HasPrefix(id, "mock-") {
		t.Errorf("instance ID should start with 'mock-', got %q", id)
	}

	observation, err := m.Observe(ctx, id)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observation.State != InstanceStateRunning {
		t.Errorf("state: got %q, want %q", observation.State, InstanceStateRunning)
	}
	if observation.Location != "us-central1-a" {
		t.Errorf("location: got %q, want %q", observation.Location, "us-central1-a")
	}
	if observation.InternalIP == "" {
		t.Error("InternalIP should not be empty")
	}
}

// TestMockComputeAdapter_UniqueIDs verifies that multiple instances get different IDs and IPs.
func TestMockComputeAdapter_UniqueIDs(t *testing.T) {
	m := NewMockComputeAdapter()
	ctx := context.Background()

	spec := MachineSpec{Name: "test", Location: "us-central1-a"}

	id1, _ := m.CreateInstance(ctx, spec)
	id2, _ := m.CreateInstance(ctx, spec)
	if id1 == id2 {
		t.Errorf("expected unique IDs, got %q twice", id1)
	}

	s1, _ := m.Observe(ctx, id1)
	s2, _ := m.Observe(ctx, id2)
	if s1.InternalIP == s2.InternalIP {
		t.Errorf("expected unique IPs, got %q twice", s1.InternalIP)
	}

	if m.InstanceCount() != 2 {
		t.Errorf("InstanceCount: got %d, want 2", m.InstanceCount())
	}
}

// TestMockComputeAdapter_StopAndStart verifies the stop/start cycle.
func TestMockComputeAdapter_StopAndStart(t *testing.T) {
	m := NewMockComputeAdapter()
	ctx := context.Background()

	id, _ := m.CreateInstance(ctx, MachineSpec{Name: "test", Location: "us-central1-a"})

	if err := m.StopInstance(ctx, id); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	status, _ := m.Observe(ctx, id)
	if status.State != InstanceStateStopped {
		t.Errorf("after stop: got %q, want %q", status.State, InstanceStateStopped)
	}

	if err := m.StartInstance(ctx, id); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	status, _ = m.Observe(ctx, id)
	if status.State != InstanceStateRunning {
		t.Errorf("after start: got %q, want %q", status.State, InstanceStateRunning)
	}
}

// TestMockComputeAdapter_Delete verifies that delete removes the instance.
func TestMockComputeAdapter_Delete(t *testing.T) {
	m := NewMockComputeAdapter()
	ctx := context.Background()

	id, _ := m.CreateInstance(ctx, MachineSpec{Name: "test", Location: "us-central1-a"})

	if err := m.DeleteInstance(ctx, id); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if m.InstanceCount() != 0 {
		t.Errorf("InstanceCount after delete: got %d, want 0", m.InstanceCount())
	}

	// Observe on deleted instance should return an error.
	_, err := m.Observe(ctx, id)
	if err == nil {
		t.Error("expected error for deleted instance, got nil")
	}
}

// TestMockComputeAdapter_Preemption verifies that one observation carries both
// the lifecycle state and provider interruption.
func TestMockComputeAdapter_Preemption(t *testing.T) {
	m := NewMockComputeAdapter()
	ctx := context.Background()

	id, _ := m.CreateInstance(ctx, MachineSpec{Name: "spot-machine", Location: "us-central1-a", Interruptible: true})

	observation, err := m.Observe(ctx, id)
	if err != nil {
		t.Fatalf("Observe before: %v", err)
	}
	if observation.Interruption != InterruptionNone {
		t.Error("expected not preempted before SetPreempted")
	}

	// Simulate GCE preemption.
	if err := m.SetPreempted(id); err != nil {
		t.Fatalf("SetPreempted: %v", err)
	}

	observation, err = m.Observe(ctx, id)
	if err != nil {
		t.Fatalf("Observe after: %v", err)
	}
	if observation.State != InstanceStateStopped {
		t.Errorf("state after preemption: got %q, want %q", observation.State, InstanceStateStopped)
	}
	if observation.Interruption != InterruptionPreempted {
		t.Error("expected preempted after SetPreempted")
	}
}

// TestMockComputeAdapter_ErrorsOnUnknownID verifies that all methods return errors
// for instance IDs that don't exist.
func TestMockComputeAdapter_ErrorsOnUnknownID(t *testing.T) {
	m := NewMockComputeAdapter()
	ctx := context.Background()
	id := "nonexistent-id"

	if err := m.StartInstance(ctx, id); err == nil {
		t.Error("StartInstance: expected error for unknown ID, got nil")
	}
	if err := m.StopInstance(ctx, id); err == nil {
		t.Error("StopInstance: expected error for unknown ID, got nil")
	}
	if err := m.DeleteInstance(ctx, id); err == nil {
		t.Error("DeleteInstance: expected error for unknown ID, got nil")
	}
	if _, err := m.Observe(ctx, id); err == nil {
		t.Error("Observe: expected error for unknown ID, got nil")
	}
	if err := m.SetPreempted(id); err == nil {
		t.Error("SetPreempted: expected error for unknown ID, got nil")
	}
}

// TestMockComputeAdapter_ConcurrentAccess verifies that concurrent calls don't
// produce data races. Run with: go test -race ./pkg/adapters/...
func TestMockComputeAdapter_ConcurrentAccess(t *testing.T) {
	m := NewMockComputeAdapter()
	ctx := context.Background()

	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			id, _ := m.CreateInstance(ctx, MachineSpec{Name: "concurrent", Location: "us-central1-a"})
			_, _ = m.Observe(ctx, id)
			_ = m.StopInstance(ctx, id)
			_ = m.DeleteInstance(ctx, id)
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestExistingCapacityProviderLifecycleDoesNotMutateHost(t *testing.T) {
	ctx := context.Background()
	provider := NewStaticComputeAdapter()
	identity := MachineIdentity{Name: "local-worker"}
	desired := DesiredMachine{Availability: DesiredOnline}

	online, err := provider.Reconcile(ctx, identity, desired, "")
	if err != nil {
		t.Fatalf("Reconcile Online: %v", err)
	}
	if online.State != CapacityPending || online.Reason != ReasonExternalWait {
		t.Fatalf("online observation = %+v, want Pending/ExternalWait", online)
	}
	if online.ProviderRef == "" {
		t.Fatal("online ProviderRef is empty")
	}
	if online.NodeSelector[MachineLabelKey] != identity.Name {
		t.Errorf("online NodeSelector = %#v, want machine label %q", online.NodeSelector, identity.Name)
	}

	desired.Availability = DesiredOffline
	offline, err := provider.Reconcile(ctx, identity, desired, online.ProviderRef)
	if err != nil {
		t.Fatalf("Reconcile Offline: %v", err)
	}
	if offline.State != CapacityOffline || offline.ProviderRef != online.ProviderRef {
		t.Fatalf("offline observation = %+v, want Offline with preserved ref", offline)
	}
	if provider.InstanceCount() != 0 {
		t.Errorf("InstanceCount() = %d, want 0; logical lifecycle must not create capacity", provider.InstanceCount())
	}

	desired.Availability = DesiredDeleted
	deleted, err := provider.Reconcile(ctx, identity, desired, online.ProviderRef)
	if err != nil {
		t.Fatalf("Reconcile Deleted: %v", err)
	}
	if deleted.State != CapacityAbsent || deleted.ProviderRef != "" {
		t.Fatalf("deleted observation = %+v, want Absent with no ref", deleted)
	}
	if provider.InstanceCount() != 0 {
		t.Errorf("InstanceCount() = %d after unregister, want 0", provider.InstanceCount())
	}
}

func TestExistingCapacityProviderCapabilities(t *testing.T) {
	provider := NewStaticComputeAdapter()
	got, err := provider.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if got.CanProvision {
		t.Error("CanProvision = true, want false")
	}
	if !got.CanDiscoverExisting {
		t.Error("CanDiscoverExisting = false, want true")
	}
	if got.SuspendMode != SuspendLogicalOnly {
		t.Errorf("SuspendMode = %q, want %q", got.SuspendMode, SuspendLogicalOnly)
	}
	if got.DeletionMode != UnregisterOnly {
		t.Errorf("DeletionMode = %q, want %q", got.DeletionMode, UnregisterOnly)
	}
	if got.SupportsInterruptible || got.SupportsLocations {
		t.Errorf("Capabilities() = %+v, existing capacity must not advertise cloud choices", got)
	}
}

// TestMockAdapter_ImplementsInterface is a compile-time assertion that MockComputeAdapter
// implements ComputeAdapter.
var _ ComputeAdapter = (*MockComputeAdapter)(nil)
