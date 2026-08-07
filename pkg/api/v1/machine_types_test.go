package v1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
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
