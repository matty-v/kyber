package v1

import "testing"

func TestPodConfigurationPending(t *testing.T) {
	agent := &Agent{}
	agent.Generation = 3
	agent.Spec.Runtime = "codex"
	agent.Spec.Model = "initial"
	agent.Status.ObservedGeneration = 3
	agent.Status.ObservedPodConfigHash = agent.PodConfigHash()
	agent.Status.JobsProjectedGeneration = 3
	if agent.PodConfigurationPending() {
		t.Fatal("current pod and jobs should not be pending")
	}

	agent.Generation = 4
	agent.Spec.Jobs = []AgentJob{{Name: "check", Schedule: "0 * * * *", Prompt: "check"}}
	if agent.PodConfigHash() != agent.Status.ObservedPodConfigHash {
		t.Fatal("hot jobs edit changed pod fingerprint")
	}
	if !agent.PodConfigurationPending() {
		t.Fatal("jobs edit must wait for in-pod projection")
	}
	if agent.PodRestartRequired() {
		t.Fatal("hot jobs edit should not require a pod restart")
	}
	agent.Status.JobsProjectedGeneration = 4
	if agent.PodConfigurationPending() {
		t.Fatal("projected jobs edit should clear pending state")
	}

	agent.Generation = 5
	agent.Spec.Model = "replacement"
	agent.Status.JobsProjectedGeneration = 5
	if !agent.PodConfigurationPending() {
		t.Fatal("model change requires a new pod even when jobs are synced")
	}
	if !agent.PodRestartRequired() {
		t.Fatal("model change should require a pod restart")
	}

	agent.Status.ObservedPodConfigHash = ""
	if !agent.PodConfigurationPending() {
		t.Fatal("legacy status should fall back to generation check")
	}
	agent.Status.ObservedGeneration = agent.Generation
	agent.Status.JobsProjectedGeneration = 0
	if agent.PodConfigurationPending() {
		t.Fatal("unmigrated current pod should retain its legacy availability")
	}
	agent.Status.ObservedGeneration = 0
	if agent.PodConfigurationPending() {
		t.Fatal("agent without a pod has no pending pod configuration")
	}
}

func TestPodConfigHashHotFields(t *testing.T) {
	agent := &Agent{}
	agent.Spec.Runtime = "codex"
	baseline := agent.PodConfigHash()
	agent.Spec.Profile.Alias = "new name"
	agent.Spec.DesiredPhase = AgentPhaseStopped
	agent.Spec.RequestReplyEnabled = true
	agent.Spec.PublicCapabilities = &AgentPublicCapabilities{}
	agent.Spec.InboundBindings = []AgentInboundBinding{}
	if agent.PodConfigHash() != baseline {
		t.Fatal("controller/API-only edits changed pod fingerprint")
	}
	agent.Spec.Runtime = "claude-code"
	if agent.PodConfigHash() == baseline {
		t.Fatal("runtime change did not change pod fingerprint")
	}
}

func TestPodConfigHashIncludesReasoningEffort(t *testing.T) {
	a := &Agent{}
	baseline := a.PodConfigHash()
	a.Spec.Effort = "high"
	if a.PodConfigHash() == baseline {
		t.Fatal("effort change did not require a new pod")
	}
	a.Spec.Effort = ""
	if a.PodConfigHash() != baseline {
		t.Fatal("clearing effort changed unrelated configuration")
	}
}
