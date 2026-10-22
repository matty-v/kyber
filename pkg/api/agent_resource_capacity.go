package api

import (
	"context"
	"fmt"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Each Agent pod can contain the status, channel, and transcript sidecars in
// addition to its main container. Keep a conservative allowance for those
// requests, including Agents which are stopped and have no pod to inspect.
var agentPodOverheadCPU = resource.MustParse("100m")
var agentPodOverheadMemory = resource.MustParse("512Mi")

// agentResourceHeadroom combines Kyber's reservations with live node usage.
// The latter includes unrelated pods. The smaller budget wins. The final
// scheduler/admission decision can still change after this snapshot.
func agentResourceHeadroom(ctx context.Context, reader client.Reader, machine *kyberv1.Machine, agents []kyberv1.Agent, excludedAgent string) (MachineAvailable, error) {
	// Recompute from the fresh Agent list. Machine.status.availableCapacity is
	// controller-published and can lag the previous API write even under a Lease.
	available := MachineAvailable{
		CPU: machine.Spec.Capacity.CPU.DeepCopy(), Memory: machine.Spec.Capacity.Memory.DeepCopy(),
	}
	if machine.Status.ObservedCapacity != nil {
		if available.CPU.IsZero() || machine.Status.ObservedCapacity.CPU.Cmp(available.CPU) < 0 {
			available.CPU = machine.Status.ObservedCapacity.CPU.DeepCopy()
		}
		if available.Memory.IsZero() || machine.Status.ObservedCapacity.Memory.Cmp(available.Memory) < 0 {
			available.Memory = machine.Status.ObservedCapacity.Memory.DeepCopy()
		}
	}
	if machine.Status.AssignableCapacity != nil {
		if machine.Status.AssignableCapacity.CPU.Cmp(available.CPU) < 0 {
			available.CPU = machine.Status.AssignableCapacity.CPU.DeepCopy()
		}
		if machine.Status.AssignableCapacity.Memory.Cmp(available.Memory) < 0 {
			available.Memory = machine.Status.AssignableCapacity.Memory.DeepCopy()
		}
	}
	if available.CPU.IsZero() || available.Memory.IsZero() {
		return MachineAvailable{}, fmt.Errorf("machine assignable capacity is unknown")
	}
	count := 1 // Proposed pod's sidecars, even when it does not exist yet.
	for i := range agents {
		if agents[i].Spec.Machine == machine.Name && agents[i].Name != excludedAgent {
			available.CPU.Sub(agents[i].Spec.Resources.CPU)
			available.Memory.Sub(agents[i].Spec.Resources.Memory)
			count++
		}
	}
	if isLiveMachinePhase(machine.Status.Phase) && machine.Status.AvailableCapacity != nil {
		published := MachineAvailable{
			CPU: machine.Status.AvailableCapacity.CPU.DeepCopy(), Memory: machine.Status.AvailableCapacity.Memory.DeepCopy(),
		}
		for i := range agents {
			if agents[i].Spec.Machine == machine.Name && agents[i].Name == excludedAgent {
				published.CPU.Add(agents[i].Spec.Resources.CPU)
				published.Memory.Add(agents[i].Spec.Resources.Memory)
				break
			}
		}
		if published.CPU.Cmp(available.CPU) < 0 {
			available.CPU = published.CPU
		}
		if published.Memory.Cmp(available.Memory) < 0 {
			available.Memory = published.Memory
		}
	}
	for range count {
		available.CPU.Sub(agentPodOverheadCPU)
		available.Memory.Sub(agentPodOverheadMemory)
	}
	if machine.Status.NodeName == "" {
		if isLiveMachinePhase(machine.Status.Phase) && machine.Spec.Provider != kyberv1.MachineProviderMock && machine.Spec.Provider != kyberv1.MachineProviderFake {
			return MachineAvailable{}, fmt.Errorf("machine node is not known yet")
		}
		return clampAgentHeadroom(available), nil
	}
	nodeHeadroom, err := nodeAvailableResources(ctx, reader, machine.Status.NodeName)
	if err != nil {
		return MachineAvailable{}, fmt.Errorf("read node capacity: %w", err)
	}
	// The old pod is removed before replacement. Add back its complete requests
	// (including sidecars), then reserve overhead for the replacement.
	if excludedAgent != "" {
		pods := &corev1.PodList{}
		if err := reader.List(ctx, pods, client.InNamespace(machine.Namespace), client.MatchingLabels{"kyber.io/agent": excludedAgent}); err != nil {
			return MachineAvailable{}, fmt.Errorf("list agent pods: %w", err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Spec.NodeName != machine.Status.NodeName || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || pod.DeletionTimestamp != nil {
				continue
			}
			for _, container := range pod.Spec.Containers {
				nodeHeadroom.CPU.Add(*container.Resources.Requests.Cpu())
				nodeHeadroom.Memory.Add(*container.Resources.Requests.Memory())
			}
			// An init container request can dominate the regular containers.
			// nodeAvailableResources subtracts it, so add it back as well.
			for _, container := range pod.Spec.InitContainers {
				nodeHeadroom.CPU.Add(*container.Resources.Requests.Cpu())
				nodeHeadroom.Memory.Add(*container.Resources.Requests.Memory())
			}
		}
	}
	nodeHeadroom.CPU.Sub(agentPodOverheadCPU)
	nodeHeadroom.Memory.Sub(agentPodOverheadMemory)
	if nodeHeadroom.CPU.Cmp(available.CPU) < 0 {
		available.CPU = nodeHeadroom.CPU
	}
	if nodeHeadroom.Memory.Cmp(available.Memory) < 0 {
		available.Memory = nodeHeadroom.Memory
	}
	return clampAgentHeadroom(available), nil
}

func clampAgentHeadroom(available MachineAvailable) MachineAvailable {
	zero := resource.MustParse("0")
	if available.CPU.Cmp(zero) < 0 {
		available.CPU = zero.DeepCopy()
	}
	if available.Memory.Cmp(zero) < 0 {
		available.Memory = zero.DeepCopy()
	}
	return available
}
