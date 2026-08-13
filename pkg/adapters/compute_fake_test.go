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
