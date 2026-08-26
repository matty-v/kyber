package machine

import (
	"context"
	"testing"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// This intentionally uses the fake Kubernetes client: the contract under test
// is controller/provider status propagation, not API-server behavior.
func TestFallbackRetryControllerContract(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme()
	provider := adapters.NewFakeComputeAdapter()
	machine := newTestMachine("fallback-contract", "default")
	machine.Spec.Provider = kyberv1.MachineProviderFake
	machine.Spec.AvailabilityClass = kyberv1.MachineAvailabilityCostOptimized
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(machine).WithObjects(machine).Build()
	r := &MachineReconciler{Client: client, Scheme: scheme, Recorder: record.NewFakeRecorder(20), CapacityProvider: provider}

	created, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOnline)
	if err != nil {
		t.Fatalf("create capacity: %v", err)
	}
	ref := created.ProviderRef
	if err := provider.ApplySimulationScenario(machine.Name, adapters.SimulationCostOptimizedUnavailable); err != nil {
		t.Fatalf("simulate fallback: %v", err)
	}
	fallback, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOnline)
	if err != nil {
		t.Fatalf("reconcile fallback: %v", err)
	}
	if fallback.ProviderRef != ref || fallback.EffectiveAvailabilityClass != "reliable" {
		t.Fatalf("fallback = %+v, want same ref %q and reliable", fallback, ref)
	}

	machine.Spec.CostOptimizedRetryRequest = "retry-success"
	retried, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOnline)
	if err != nil {
		t.Fatalf("reconcile retry: %v", err)
	}
	if retried.ProviderRef != ref || retried.EffectiveAvailabilityClass != "costOptimized" || retried.CostOptimizedRetryObserved != "retry-success" {
		t.Fatalf("successful retry = %+v", retried)
	}

	if err := provider.ApplySimulationScenario(machine.Name, adapters.SimulationCostOptimizedUnavailable); err != nil {
		t.Fatalf("simulate second fallback: %v", err)
	}
	if err := provider.ApplySimulationScenario(machine.Name, adapters.SimulationFailNextCostOptimizedRetry); err != nil {
		t.Fatalf("simulate failed retry: %v", err)
	}
	machine.Spec.CostOptimizedRetryRequest = "retry-rollback"
	rolledBack, err := r.reconcileCapacity(ctx, machine, adapters.DesiredOnline)
	if err != nil {
		t.Fatalf("reconcile rollback: %v", err)
	}
	if rolledBack.ProviderRef != ref || rolledBack.EffectiveAvailabilityClass != "reliable" || rolledBack.CostOptimizedRetryObserved != "retry-rollback" {
		t.Fatalf("rollback = %+v, want same ref and reliable", rolledBack)
	}
}
