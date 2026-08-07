package api

import (
	"k8s.io/apimachinery/pkg/api/resource"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// MachineAvailable is the resources a new agent can still request on a Machine.
// Clamps against both the declared Spec.Capacity and the live Status.ObservedCapacity
// (whichever is smaller), then subtracts the sum of existing agent requests
// that target this Machine. Returns zero quantities when fully subscribed.
type MachineAvailable struct {
	CPU    resource.Quantity
	Memory resource.Quantity
}

// machineAvailable computes the remaining budget. agents may be the full
// cross-machine list — entries with Spec.Machine != m.Name are ignored.
//
// Preferred path (post-#140): when the machine is in a live phase
// (Provisioning, Ready, Running) and the controller has populated
// Status.AvailableCapacity, return that directly. The machine controller
// publishes this on every reconcile.
//
// Fallback path: when the machine is in a non-live phase (Failed, Stopping,
// Stopped, Preempted, Replacing) the published Status.AvailableCapacity is
// potentially stale — the state machine transitions to those phases via
// actions that do not re-run capacity discovery (e.g. ActionMarkFailed when
// a node disappears). Compute on the fly against Spec.Capacity, clamped by
// Status.ObservedCapacity if present, minus the cross-machine agent list.
// Same algorithm as the original pre-#140 behavior but reading the renamed
// ObservedCapacity field instead of the old Allocatable.
//
// Same fallback applies during the in-flight controller upgrade window
// where Status.AvailableCapacity is nil because no reconcile has run yet.
func machineAvailable(m *kyberv1.Machine, agents []kyberv1.Agent) MachineAvailable {
	// Preferred: status field set by the controller on a live machine.
	if isLiveMachinePhase(m.Status.Phase) && m.Status.AvailableCapacity != nil {
		return MachineAvailable{
			CPU:    m.Status.AvailableCapacity.CPU.DeepCopy(),
			Memory: m.Status.AvailableCapacity.Memory.DeepCopy(),
		}
	}

	// Fallback: clamp Spec.Capacity by ObservedCapacity, subtract agents.
	cpu := m.Spec.Capacity.CPU.DeepCopy()
	mem := m.Spec.Capacity.Memory.DeepCopy()

	if m.Status.ObservedCapacity != nil {
		if cpu.IsZero() {
			cpu = m.Status.ObservedCapacity.CPU.DeepCopy()
		} else if m.Status.ObservedCapacity.CPU.Cmp(cpu) < 0 {
			cpu = m.Status.ObservedCapacity.CPU.DeepCopy()
		}
		if mem.IsZero() {
			mem = m.Status.ObservedCapacity.Memory.DeepCopy()
		} else if m.Status.ObservedCapacity.Memory.Cmp(mem) < 0 {
			mem = m.Status.ObservedCapacity.Memory.DeepCopy()
		}
	}

	for i := range agents {
		if agents[i].Spec.Machine != m.Name {
			continue
		}
		cpu.Sub(agents[i].Spec.Resources.CPU)
		mem.Sub(agents[i].Spec.Resources.Memory)
	}

	zero := resource.MustParse("0")
	if cpu.Cmp(zero) < 0 {
		cpu = zero.DeepCopy()
	}
	if mem.Cmp(zero) < 0 {
		mem = zero.DeepCopy()
	}

	return MachineAvailable{CPU: cpu, Memory: mem}
}

// isLiveMachinePhase reports whether the machine's current phase indicates a
// live, capacity-bearing node. Phases like Failed/Stopping/Stopped/Preempted/
// Replacing keep their last-published Status.AvailableCapacity even though
// the underlying node may be gone — so the API consumer must NOT trust that
// field in those phases. See #140 + the Task 3 review notes.
func isLiveMachinePhase(phase kyberv1.MachinePhase) bool {
	switch phase {
	case kyberv1.MachinePhaseProvisioning,
		kyberv1.MachinePhaseReady,
		kyberv1.MachinePhaseRunning:
		return true
	}
	return false
}

// MachineAvailableExcluding is machineAvailable minus one named agent's current
// reservation. Use during an in-place resize so the requesting agent's current
// allocation is treated as returnable headroom (we're about to overwrite it).
// If excludeName doesn't match any agent on this machine, the result equals
// machineAvailable.
//
// Post-#140: when the machine is in a live phase and the controller has
// populated Status.AvailableCapacity, that field already has the excluded
// agent's allocation subtracted. We add it back here so the resize-self
// path sees the correct headroom. Otherwise (non-live phase or pre-first-
// reconcile), we filter the agent list and let the legacy fallback path in
// machineAvailable do the math.
func MachineAvailableExcluding(m *kyberv1.Machine, agents []kyberv1.Agent, excludeName string) MachineAvailable {
	// Preferred path: live machine with controller-published AvailableCapacity.
	// Status field already excludes ALL bound agents — we add back the excluded
	// one if it's bound to this machine.
	if isLiveMachinePhase(m.Status.Phase) && m.Status.AvailableCapacity != nil {
		cpu := m.Status.AvailableCapacity.CPU.DeepCopy()
		mem := m.Status.AvailableCapacity.Memory.DeepCopy()
		for i := range agents {
			if agents[i].Name == excludeName && agents[i].Spec.Machine == m.Name {
				cpu.Add(agents[i].Spec.Resources.CPU)
				mem.Add(agents[i].Spec.Resources.Memory)
				break
			}
		}
		return MachineAvailable{CPU: cpu, Memory: mem}
	}

	// Fallback: filter excluded agent and let machineAvailable do the on-the-fly
	// compute. Same pre-#140 behavior, now reading the renamed ObservedCapacity.
	filtered := make([]kyberv1.Agent, 0, len(agents))
	for i := range agents {
		if agents[i].Name == excludeName && agents[i].Spec.Machine == m.Name {
			continue
		}
		filtered = append(filtered, agents[i])
	}
	return machineAvailable(m, filtered)
}
