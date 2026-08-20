package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/adapters"
	"github.com/matty-v/kyber/pkg/api"
)

type discoverableFakeProvider struct {
	*adapters.FakeComputeAdapter
}

func (p *discoverableFakeProvider) Capabilities(context.Context) (adapters.Capabilities, error) {
	return adapters.Capabilities{
		CanProvision: true, CanDiscoverExisting: true,
		SuspendMode: adapters.SuspendCapacity, DeletionMode: adapters.DeleteCapacity,
		SupportsReliable: true, SupportsInterruptible: true, SupportsLocations: true,
	}, nil
}

func TestMachinePreflightResolvesNeutralIntent(t *testing.T) {
	s := &api.Server{
		K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).Build(),
		APIKey:    testAPIKey, Namespace: "kyber-system", ComputeProvider: "fake",
		CapacityProvider: adapters.NewFakeComputeAdapter(),
	}
	req := authedRequest(t, http.MethodPost, "/api/v1/machines/preflight", map[string]interface{}{
		"provider": "fake", "profile": "e2-small", "diskSizeGb": 20,
		"location": "local-a", "availabilityClass": "costOptimized",
	})
	rr := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got api.MachinePreflightResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Valid || got.Resolved.Profile != "e2-small" || got.Resolved.AvailabilityClass != "costOptimized" || got.Resolved.ManagementMode != "Managed" {
		t.Fatalf("resolved = %+v", got.Resolved)
	}
}

func TestMachinePreflightAllowsExplicitExternalMode(t *testing.T) {
	s := &api.Server{
		K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).Build(),
		APIKey:    testAPIKey, Namespace: "kyber-system", ComputeProvider: "gke",
		CapacityProvider: &discoverableFakeProvider{FakeComputeAdapter: adapters.NewFakeComputeAdapter()},
	}
	req := authedRequest(t, http.MethodPost, "/api/v1/machines/preflight", map[string]interface{}{
		"provider": "gke", "managementMode": "External",
	})
	rr := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got api.MachinePreflightResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Resolved.ManagementMode != "External" {
		t.Fatalf("managementMode = %q, want External", got.Resolved.ManagementMode)
	}
}

func TestMachineCandidatesExcludePlatformAndClaimedNodes(t *testing.T) {
	ready := func(name string, taints []corev1.Taint) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelTopologyZone: "local-a"}},
			Spec:       corev1.NodeSpec{Taints: taints},
			Status: corev1.NodeStatus{
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi")},
			},
		}
	}
	client := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(
		ready("candidate", nil),
		ready("platform", []corev1.Taint{{Key: "kyber.io/platform", Effect: corev1.TaintEffectNoSchedule}}),
	).Build()
	s := &api.Server{
		K8sClient: client, APIKey: testAPIKey, Namespace: "kyber-system",
		ComputeProvider: "static", CapacityProvider: adapters.NewMockComputeAdapter(),
	}
	req := authedRequest(t, http.MethodGet, "/api/v1/machine-candidates", nil)
	rr := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("candidates: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got api.MachineCandidatesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].DisplayName != "candidate" || got.Items[0].ID == "candidate" {
		t.Fatalf("candidates = %+v", got.Items)
	}
}
