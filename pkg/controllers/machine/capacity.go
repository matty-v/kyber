package machine

import (
	"k8s.io/apimachinery/pkg/api/resource"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// ComputeAssignable returns observed minus reservation, floored at zero per
// resource. Used to derive Machine.status.assignableCapacity from the backing
// node's reported allocatable and the chart-configured platform reservation.
//
// EphemeralStorage is included since #129 PR-C — the zero value is preserved
// when observed.EphemeralStorage is also zero (pre-PR-C nodes), so older
// reservation configs that don't set ephemeralStorage stay backward-compat.
func ComputeAssignable(observed, reservation kyberv1.MachineCapacity) kyberv1.MachineCapacity {
	cpu := observed.CPU.DeepCopy()
	mem := observed.Memory.DeepCopy()
	disk := observed.EphemeralStorage.DeepCopy()
	cpu.Sub(reservation.CPU)
	mem.Sub(reservation.Memory)
	disk.Sub(reservation.EphemeralStorage)

	zero := resource.MustParse("0")
	if cpu.Cmp(zero) < 0 {
		cpu = zero.DeepCopy()
	}
	if mem.Cmp(zero) < 0 {
		mem = zero.DeepCopy()
	}
	if disk.Cmp(zero) < 0 {
		disk = zero.DeepCopy()
	}
	return kyberv1.MachineCapacity{CPU: cpu, Memory: mem, EphemeralStorage: disk}
}

// ComputeAvailable returns assignable minus the sum of resource requests for
// all agents bound to machineName, floored at zero per resource. Agents on
// other machines are ignored. Used to derive Machine.status.availableCapacity.
func ComputeAvailable(assignable kyberv1.MachineCapacity, agents []kyberv1.Agent, machineName string) kyberv1.MachineCapacity {
	cpu := assignable.CPU.DeepCopy()
	mem := assignable.Memory.DeepCopy()
	disk := assignable.EphemeralStorage.DeepCopy()

	for i := range agents {
		if agents[i].Spec.Machine != machineName {
			continue
		}
		cpu.Sub(agents[i].Spec.Resources.CPU)
		mem.Sub(agents[i].Spec.Resources.Memory)
		disk.Sub(agents[i].Spec.Resources.Disk)
	}

	zero := resource.MustParse("0")
	if cpu.Cmp(zero) < 0 {
		cpu = zero.DeepCopy()
	}
	if mem.Cmp(zero) < 0 {
		mem = zero.DeepCopy()
	}
	if disk.Cmp(zero) < 0 {
		disk = zero.DeepCopy()
	}
	return kyberv1.MachineCapacity{CPU: cpu, Memory: mem, EphemeralStorage: disk}
}
