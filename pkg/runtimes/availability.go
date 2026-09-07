package runtimes

import (
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"time"
)

const ObservationTTL = 90 * time.Second

type Availability struct {
	Supported bool   `json:"supported"`
	State     string `json:"state"` // available, unavailable, unknown
	Reason    string `json:"reason"`
}

// AvailabilityFor combines trusted declarations with expiring pod evidence.
// It never interprets heartbeat or process readiness as successful provider auth.
func AvailabilityFor(agent *kyberv1.Agent, feature Feature, now time.Time) Availability {
	result := Availability{State: "unavailable", Reason: "unsupported"}
	d, ok := Describe(agent.Spec.Runtime)
	if !ok || !d.Supports(feature) {
		return result
	}
	result.Supported = true
	if agent.Status.Phase != kyberv1.AgentPhaseRunning {
		result.Reason = "agent_not_running"
		return result
	}
	if agent.SpecChangedSinceLastPod() {
		result.Reason = "configuration_pending"
		return result
	}
	if agent.Status.Runtime.Usable != nil && !*agent.Status.Runtime.Usable {
		result.Reason = "runtime_unusable"
		return result
	}
	// Hook-based features require positive, current boot evidence. Other
	// command features still perform their existing in-pod checks when invoked.
	if feature == JobTurnHooks || feature == TaskReceipts || feature == TaskTools {
		evidence := agent.Status.Runtime.Capabilities
		if evidence == nil || evidence.ContractVersion != ContractVersion || evidence.InstalledVersion != agent.Status.Runtime.InstalledVersion || now.Sub(evidence.ObservedAt.Time) > ObservationTTL || evidence.ObservedAt.Time.After(now.Add(5*time.Second)) {
			result.State = "unknown"
			result.Reason = "evidence_missing_or_stale"
			return result
		}
		available, present := evidence.Features[string(feature)]
		if !present {
			result.State = "unknown"
			result.Reason = "feature_not_observed"
			return result
		}
		if !available {
			result.Reason = "integration_unavailable"
			return result
		}
	}
	result.State = "available"
	result.Reason = "supported"
	return result
}
func AvailabilityMap(agent *kyberv1.Agent, now time.Time) map[Feature]Availability {
	result := map[Feature]Availability{}
	for _, f := range []Feature{SessionRestart, SessionResume, Compaction, JobTurnHooks, TaskReceipts, TaskTools, ModelCatalog, UsageReporting, RuntimeRepairFeature} {
		result[f] = AvailabilityFor(agent, f, now)
	}
	return result
}
