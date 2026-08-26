package v1

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMachineSpec_CapacityRoundTrip(t *testing.T) {
	orig := MachineSpec{
		Provider: MachineProviderMock,
		Capacity: MachineCapacity{
			CPU:    resource.MustParse("4"),
			Memory: resource.MustParse("16Gi"),
		},
	}
	clone := orig.DeepCopy()

	// Mutate the original — the clone must not follow.
	orig.Capacity.CPU = resource.MustParse("8")
	orig.Capacity.Memory = resource.MustParse("32Gi")
	orig.Provider = MachineProviderGCE

	if got := clone.Capacity.CPU.String(); got != "4" {
		t.Errorf("cpu after mutate-orig = %q, want %q (alias leak)", got, "4")
	}
	if got := clone.Capacity.Memory.String(); got != "16Gi" {
		t.Errorf("memory after mutate-orig = %q, want %q (alias leak)", got, "16Gi")
	}
	if clone.Provider != MachineProviderMock {
		t.Errorf("provider after mutate-orig = %q, want %q (alias leak)", clone.Provider, MachineProviderMock)
	}
}

func TestMachineStatus_FallbackTimesDeepCopy(t *testing.T) {
	fallback := metav1.NewTime(time.Unix(100, 0))
	unavailable := metav1.NewTime(time.Unix(200, 0))
	orig := MachineStatus{
		FallbackSince:                 &fallback,
		CostOptimizedUnavailableSince: &unavailable,
	}
	clone := orig.DeepCopy()
	orig.FallbackSince.Time = time.Unix(300, 0)
	orig.CostOptimizedUnavailableSince.Time = time.Unix(400, 0)

	if clone.FallbackSince == nil || clone.FallbackSince.Unix() != 100 {
		t.Fatalf("fallbackSince clone = %v, want unix 100", clone.FallbackSince)
	}
	if clone.CostOptimizedUnavailableSince == nil || clone.CostOptimizedUnavailableSince.Unix() != 200 {
		t.Fatalf("unavailableSince clone = %v, want unix 200", clone.CostOptimizedUnavailableSince)
	}
}

func TestMachineStatus_ResolvedProfileDeepCopy(t *testing.T) {
	orig := MachineStatus{ResolvedProfile: &ResolvedMachineProfile{
		ID: "standard",
		Capacity: MachineCapacity{
			CPU: resource.MustParse("4"), Memory: resource.MustParse("16Gi"),
		},
	}}
	clone := orig.DeepCopy()
	orig.ResolvedProfile.ID = "changed"
	orig.ResolvedProfile.Capacity.CPU = resource.MustParse("8")

	if clone.ResolvedProfile == nil || clone.ResolvedProfile.ID != "standard" {
		t.Fatalf("resolved profile clone = %+v", clone.ResolvedProfile)
	}
	if got := clone.ResolvedProfile.Capacity.CPU.String(); got != "4" {
		t.Errorf("resolved profile CPU = %q, want 4", got)
	}
}
