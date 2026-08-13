package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestComputeSimulationRoutesAreExplicitlyGated(t *testing.T) {
	server := &Server{APIKey: "test"}
	rr := httptest.NewRecorder()
	server.BuildHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/dev/compute/instances", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("without API key status = %d", rr.Code)
	}

	adapter := adapters.NewFakeComputeAdapter()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	machine := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "test"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
	server = &Server{APIKey: "test", Namespace: "test", K8sClient: k8sClient, ComputeSimulation: adapter}
	id, err := adapter.CreateInstance(context.Background(), adapters.MachineSpec{Name: "worker", Location: "local-a"})
	if err != nil || id == "" {
		t.Fatalf("CreateInstance: id=%q err=%v", id, err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/dev/compute/scenarios", bytes.NewBufferString(`{"machine":"worker","scenario":"preempted"}`))
	req.Header.Set("Authorization", "Bearer test")
	rr = httptest.NewRecorder()
	server.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("scenario status = %d body=%s", rr.Code, rr.Body.String())
	}
	observation, err := adapter.Observe(context.Background(), id)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observation.Interruption != adapters.InterruptionPreempted {
		t.Fatalf("interruption = %s", observation.Interruption)
	}
	got := &kyberv1.Machine{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "test", Name: "worker"}, got); err != nil {
		t.Fatalf("Get Machine: %v", err)
	}
	if got.Annotations["kyber.io/dev-scenario-revision"] == "" {
		t.Fatal("scenario did not trigger Machine reconcile annotation")
	}
}
