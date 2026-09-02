package capabilities

import (
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/skillscan"
)

const SkillEvidenceMaxAge = 24 * time.Hour

// PlatformState is installation-owned evidence for the task contracts that a
// runtime adapter can consume. Zero values fail closed: an adapter supporting
// a feature does not make that feature usable when the corresponding Kyber
// service or backend is disabled.
type PlatformState struct {
	TasksEnabled     bool
	DurableTaskStore bool
	TaskObjectStore  bool
}

// AdapterMatrixVersion pins the internal observation vocabulary. It is not a
// public capability claim: it only limits which operator declarations current
// Kyber runtime adapters can mark available.
const AdapterMatrixVersion = "v1"

var adapterFeatureMatrix = map[string]map[string]bool{
	"claude-code": {"durable": true, "progress": true, "typed-results": true, "files": true, "cancellation": true, "multi-turn": true, "authorization-request": true},
	"codex":       {"durable": true, "progress": true, "typed-results": true, "files": true, "cancellation": true, "multi-turn": true, "authorization-request": true},
}

// Evaluate joins an explicit declaration with bounded private observations.
// Observations may narrow availability but can never create a capability.
func Evaluate(agent *kyberv1.Agent, report *skillscan.Report, reportErr error, platform PlatformState, now time.Time) *kyberv1.AgentPublicCapabilitiesStatus {
	if agent.Spec.PublicCapabilities == nil {
		return nil
	}
	_, digest, validationErr := NormalizeAndValidate(agent.Spec.PublicCapabilities)
	status := &kyberv1.AgentPublicCapabilitiesStatus{
		ObservedGeneration: agent.Generation,
		ManifestRevision:   digest,
		ObservedAt:         &metav1.Time{Time: now.UTC()},
		Capabilities:       make([]kyberv1.AgentPublicCapabilityAvailability, 0, len(agent.Spec.PublicCapabilities.Capabilities)),
	}
	if validationErr != nil {
		status.Conditions = []metav1.Condition{
			condition("Valid", metav1.ConditionFalse, "InvalidDeclaration", validationErr.Error(), agent.Generation, now),
			condition("Available", metav1.ConditionFalse, "InvalidDeclaration", "invalid manifests are not published", agent.Generation, now),
			condition("Drift", metav1.ConditionUnknown, "InvalidDeclaration", "drift cannot be evaluated for an invalid declaration", agent.Generation, now),
		}
		return status
	}
	status.Conditions = append(status.Conditions, condition("Valid", metav1.ConditionTrue, "Validated", "public capability declaration is valid", agent.Generation, now))
	allAvailable := true
	for _, declared := range agent.Spec.PublicCapabilities.Capabilities {
		availability, reason := evaluateCapability(declared, agent, report, reportErr, platform, now)
		if availability != "available" {
			allAvailable = false
		}
		status.Capabilities = append(status.Capabilities, kyberv1.AgentPublicCapabilityAvailability{ID: declared.ID, Availability: availability, Reason: reason})
	}
	sort.Slice(status.Capabilities, func(i, j int) bool { return status.Capabilities[i].ID < status.Capabilities[j].ID })
	if allAvailable {
		status.Conditions = append(status.Conditions, condition("Available", metav1.ConditionTrue, "AllCapabilitiesAvailable", "all declared capabilities are available", agent.Generation, now))
		status.Conditions = append(status.Conditions, condition("Drift", metav1.ConditionFalse, "NoDrift", "declared capability evidence matches current observations", agent.Generation, now))
	} else {
		status.Conditions = append(status.Conditions, condition("Available", metav1.ConditionFalse, "CapabilityUnavailable", "one or more declared capabilities are not available", agent.Generation, now))
		status.Conditions = append(status.Conditions, condition("Drift", metav1.ConditionTrue, "EvidenceMismatch", "one or more declarations do not match current observations", agent.Generation, now))
	}
	return status
}

func evaluateCapability(declared kyberv1.AgentPublicCapability, agent *kyberv1.Agent, report *skillscan.Report, reportErr error, platform PlatformState, now time.Time) (string, string) {
	runtime, phase := agent.Spec.Runtime, agent.Status.Phase
	if agent.Status.Runtime.Runtime == "" || agent.Status.Runtime.Usable == nil {
		return "unknown", "runtime-evidence-unavailable"
	}
	if agent.Status.Runtime.Runtime != runtime {
		return "unavailable", "runtime-adapter-mismatch"
	}
	if !*agent.Status.Runtime.Usable {
		return "unavailable", "runtime-adapter-unusable"
	}
	supportedFeatures, runtimeSupported := adapterFeatureMatrix[runtime]
	if !runtimeSupported {
		return "unavailable", "runtime-adapter-unsupported"
	}
	if phase != kyberv1.AgentPhaseRunning {
		return "unavailable", "agent-not-running"
	}
	for _, feature := range declared.TaskFeatures {
		if feature == "event-replay" {
			return "unavailable", "platform-feature-disabled"
		}
		if !supportedFeatures[feature] {
			return "unavailable", "runtime-feature-unsupported"
		}
		if available, reason := platformFeatureAvailable(feature, platform); !available {
			return "unavailable", reason
		}
	}
	if len(declared.TaskFeatures) > 0 && !agent.Spec.RequestReplyEnabled {
		return "unavailable", "task-interface-disabled"
	}
	evidence := declared.Evidence
	if evidence == nil {
		return "available", ""
	}
	for _, connector := range evidence.RequiredConnectors {
		if !connectorAvailable(agent, connector) {
			return "unavailable", "required-connector-missing"
		}
	}
	for _, feature := range evidence.RequiredPlatformFeatures {
		if !knownFeatures[feature] || feature == "event-replay" {
			return "unavailable", "platform-feature-disabled"
		}
		if available, reason := platformFeatureAvailable(feature, platform); !available {
			return "unavailable", reason
		}
	}
	if len(evidence.RequiredPlatformFeatures) > 0 && !agent.Spec.RequestReplyEnabled {
		return "unavailable", "task-interface-disabled"
	}
	if len(evidence.RuntimeAdapters) > 0 {
		matched := false
		for _, allowed := range evidence.RuntimeAdapters {
			if allowed == runtime {
				matched = true
				break
			}
		}
		if !matched {
			return "unavailable", "runtime-adapter-mismatch"
		}
	}
	if len(evidence.RequiredSkills) == 0 {
		return "available", ""
	}
	if reportErr != nil || report == nil {
		return "unknown", "skill-evidence-unavailable"
	}
	if reportedAt, err := time.Parse(time.RFC3339, report.ReportedAt); err != nil || now.Sub(reportedAt) > SkillEvidenceMaxAge {
		return "unknown", "skill-evidence-stale"
	}
	byName := make(map[string]skillscan.Skill, len(report.Skills))
	for _, skill := range report.Skills {
		byName[skill.Name] = skill
	}
	for _, required := range evidence.RequiredSkills {
		skill, ok := byName[required]
		if !ok {
			return "unavailable", "required-skill-missing"
		}
		if skill.Broken() {
			return "unavailable", "required-skill-broken"
		}
		linked := false
		for _, linkedRuntime := range skill.Linked {
			if linkedRuntime == runtime {
				linked = true
				break
			}
		}
		if !linked {
			return "unavailable", "required-skill-unlinked"
		}
	}
	return "available", ""
}

func platformFeatureAvailable(feature string, platform PlatformState) (bool, string) {
	if !platform.TasksEnabled {
		return false, "platform-task-service-disabled"
	}
	if !platform.DurableTaskStore {
		return false, "platform-task-store-unavailable"
	}
	if feature == "files" && !platform.TaskObjectStore {
		return false, "platform-object-store-unavailable"
	}
	return true, ""
}

func connectorAvailable(agent *kyberv1.Agent, connector string) bool {
	switch connector {
	case "github":
		return agent.Spec.IdentityRepo.Repo != "" && agent.Status.IdentityRepo.Phase == kyberv1.AgentIdentityRepoPhaseReady
	case "telegram":
		return agent.Spec.Secrets.TelegramEnabled
	case "discord":
		return agent.Spec.Secrets.DiscordEnabled || (agent.Spec.Channels != nil && agent.Spec.Channels.Discord != nil)
	default:
		return false
	}
}

func condition(kind string, value metav1.ConditionStatus, reason, message string, generation int64, now time.Time) metav1.Condition {
	return metav1.Condition{Type: kind, Status: value, Reason: reason, Message: fmt.Sprintf("%s", message), ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now.UTC())}
}
