package api

import (
	"fmt"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	agentCPUStepMilli    = int64(50)
	agentMemoryStepBytes = int64(128 * 1024 * 1024)
)

// validateAgentCPU and validateAgentMemory implement the operator-facing grid.
// The Kubernetes quantity parser alone accepts values that are technically
// representable but too precise for Kyber's resource controls.
func validateAgentCPU(q resource.Quantity) error {
	if q.Cmp(setResourcesMinCPU) < 0 {
		return fmt.Errorf("cpu is below the minimum of %s", setResourcesMinCPU.String())
	}
	milli := q.MilliValue()
	if milli%agentCPUStepMilli != 0 || q.Cmp(*resource.NewMilliQuantity(milli, resource.DecimalSI)) != 0 {
		return fmt.Errorf("cpu must be a multiple of %dm", agentCPUStepMilli)
	}
	return nil
}

func validateAgentMemory(q resource.Quantity) error {
	if q.Cmp(setResourcesMinMemory) < 0 {
		return fmt.Errorf("memory is below the minimum of %s", setResourcesMinMemory.String())
	}
	bytes := q.Value()
	if bytes%agentMemoryStepBytes != 0 || q.Cmp(*resource.NewQuantity(bytes, resource.BinarySI)) != 0 {
		return fmt.Errorf("memory must be a multiple of 128Mi")
	}
	return nil
}

// applyResourceEditIntent preserves dormant and safety-gated phases. A resource
// edit is not a Start command for a stopped or unauthenticated Agent.
func applyResourceEditIntent(agent *kyberv1.Agent) error {
	if agent.DeletionTimestamp != nil {
		return fmt.Errorf("agent is being deleted")
	}
	if archiveHeld(agent) {
		return nil
	}
	if agent.Spec.DesiredPhase == kyberv1.AgentPhaseStopped && agent.Status.Phase != kyberv1.AgentPhaseStopped {
		return fmt.Errorf("agent is stopping; retry once stopped")
	}
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseRunning:
		if agent.Spec.DesiredPhase != "" && agent.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
			return fmt.Errorf("agent has a pending %s transition; retry after it settles", agent.Spec.DesiredPhase)
		}
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	case kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseMemoryExhausted:
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	case kyberv1.AgentPhaseStopped:
		if agent.Spec.DesiredPhase != kyberv1.AgentPhaseStopped {
			return fmt.Errorf("agent has a pending start; retry after it settles")
		}
	case kyberv1.AgentPhaseNeedsAuth,
		kyberv1.AgentPhaseDiskExhausted, kyberv1.AgentPhaseWaitingForMachine:
		// Keep the current intent. The new limits land on the next permitted pod.
	default:
		return fmt.Errorf("resource changes cannot be applied while the agent is %s; retry after the transition", agent.Status.Phase)
	}
	return nil
}
