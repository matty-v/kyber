package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

type retryCapableProvider struct {
	*adapters.FakeComputeAdapter
	mode adapters.ReliableFallbackMode
}

func (p *retryCapableProvider) Capabilities(ctx context.Context) (adapters.Capabilities, error) {
	capabilities, err := p.FakeComputeAdapter.Capabilities(ctx)
	capabilities.ReliableFallbackMode = p.mode
	return capabilities, err
}

func retryMachine() *kyberv1.Machine {
	return &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider:          kyberv1.MachineProviderFake,
			AvailabilityClass: kyberv1.MachineAvailabilityCostOptimized,
			ManagementMode:    kyberv1.MachineManagementManaged,
		},
		Status: kyberv1.MachineStatus{
			Phase:                      kyberv1.MachinePhaseRunning,
			EffectiveAvailabilityClass: kyberv1.MachineAvailabilityReliable,
		},
	}
}

func retryHandler(t *testing.T, machine *kyberv1.Machine, mode adapters.ReliableFallbackMode) (*Server, http.Handler) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kyber scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&kyberv1.Machine{}).WithObjects(machine).Build()
	server := &Server{
		K8sClient: k8sClient,
		APIKey:    "test-key",
		Namespace: "kyber-system",
		CapacityProvider: &retryCapableProvider{
			FakeComputeAdapter: adapters.NewFakeComputeAdapter(),
			mode:               mode,
		},
	}
	return server, server.BuildHandler()
}

func retryRequest(requestID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/machines/worker-1/retry-cost-optimized", bytes.NewBufferString(`{"requestId":"`+requestID+`"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestRetryCostOptimized_AcceptsAndIsIdempotent(t *testing.T) {
	machine := retryMachine()
	server, handler := retryHandler(t, machine, adapters.ReliableFallbackAutomatic)

	for attempt, wantStatus := range []int{http.StatusAccepted, http.StatusOK} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, retryRequest("req-1"))
		if rr.Code != wantStatus {
			t.Fatalf("attempt %d status = %d, want %d; body=%s", attempt+1, rr.Code, wantStatus, rr.Body.String())
		}
	}

	got := &kyberv1.Machine{}
	if err := server.K8sClient.Get(context.Background(), types.NamespacedName{Name: machine.Name, Namespace: machine.Namespace}, got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.Spec.CostOptimizedRetryRequest != "req-1" {
		t.Fatalf("retry request = %q, want req-1", got.Spec.CostOptimizedRetryRequest)
	}
}

func TestRetryCostOptimized_RejectsUnsupportedProvider(t *testing.T) {
	_, handler := retryHandler(t, retryMachine(), adapters.ReliableFallbackUnsupported)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, retryRequest("req-1"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRetryCostOptimized_RejectsConcurrentRequest(t *testing.T) {
	machine := retryMachine()
	machine.Spec.CostOptimizedRetryRequest = "req-old"
	_, handler := retryHandler(t, machine, adapters.ReliableFallbackManual)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, retryRequest("req-new"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRetryCostOptimized_RejectsWrongState(t *testing.T) {
	for _, mutate := range []func(*kyberv1.Machine){
		func(machine *kyberv1.Machine) { machine.Spec.AvailabilityClass = kyberv1.MachineAvailabilityReliable },
		func(machine *kyberv1.Machine) { machine.Spec.ManagementMode = kyberv1.MachineManagementExternal },
		func(machine *kyberv1.Machine) {
			machine.Status.EffectiveAvailabilityClass = kyberv1.MachineAvailabilityCostOptimized
		},
		func(machine *kyberv1.Machine) { machine.Status.Phase = kyberv1.MachinePhaseStopping },
	} {
		machine := retryMachine()
		mutate(machine)
		_, handler := retryHandler(t, machine, adapters.ReliableFallbackAutomatic)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, retryRequest("req-1"))
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
		}
	}
}
