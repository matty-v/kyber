package gceemulator_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"google.golang.org/api/option"

	"github.com/matty-v/kyber/pkg/adapters"
	"github.com/matty-v/kyber/pkg/gceemulator"
)

func TestEmulatorExercisesRealGCEAdapter(t *testing.T) {
	emulator := gceemulator.New()
	server := httptest.NewServer(emulator)
	defer server.Close()
	adapter, err := adapters.NewGCEAdapter(context.Background(), "local-project", option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("NewGCEAdapter: %v", err)
	}
	defer adapter.Close()

	id, err := adapter.CreateInstance(context.Background(), adapters.MachineSpec{Name: "manual", Profile: "e2-small", Location: "local-a", DiskSizeGb: 20, Interruptible: true})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if id == "" {
		t.Fatal("CreateInstance returned empty ID")
	}
	observation, err := adapter.Observe(context.Background(), id)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observation.State != adapters.InstanceStateRunning || observation.Location != "local-a" {
		t.Fatalf("observation = %+v", observation)
	}
	if err := emulator.ApplySimulationScenario("manual", adapters.SimulationPreempted); err != nil {
		t.Fatalf("preempt: %v", err)
	}
	observation, err = adapter.Observe(context.Background(), id)
	if err != nil {
		t.Fatalf("Observe preempted: %v", err)
	}
	if observation.Interruption != adapters.InterruptionPreempted {
		t.Fatalf("interruption = %s", observation.Interruption)
	}
	if err := adapter.StartInstance(context.Background(), id); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	if err := adapter.StopInstance(context.Background(), id); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if err := adapter.DeleteInstance(context.Background(), id); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

func TestEmulatorInjectsGCEAPIFailure(t *testing.T) {
	emulator := gceemulator.New()
	server := httptest.NewServer(emulator)
	defer server.Close()
	adapter, err := adapters.NewGCEAdapter(context.Background(), "local-project", option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("NewGCEAdapter: %v", err)
	}
	defer adapter.Close()
	if err := emulator.ApplySimulationScenario("", adapters.SimulationFailNextCreate); err != nil {
		t.Fatalf("scenario: %v", err)
	}
	_, err = adapter.CreateInstance(context.Background(), adapters.MachineSpec{Name: "failure", Profile: "e2-small", Location: "local-a", DiskSizeGb: 20})
	if err == nil {
		t.Fatal("CreateInstance succeeded after fail-next-create")
	}
}
