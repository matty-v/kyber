package agent

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/capabilities"
	"github.com/matty-v/kyber/pkg/skillscan"
)

func TestPublicCapabilityStatusEqualIgnoresOnlyTimestamps(t *testing.T) {
	a := &kyberv1.AgentPublicCapabilitiesStatus{ObservedGeneration: 2, ManifestRevision: "sha256:a", ObservedAt: &metav1.Time{Time: time.Unix(1, 0)}, Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "Validated", Message: "valid", ObservedGeneration: 2, LastTransitionTime: metav1.Time{Time: time.Unix(1, 0)}}}}
	b := &kyberv1.AgentPublicCapabilitiesStatus{ObservedGeneration: 2, ManifestRevision: "sha256:a", ObservedAt: &metav1.Time{Time: time.Unix(2, 0)}, Conditions: []metav1.Condition{{Type: "Valid", Status: metav1.ConditionTrue, Reason: "Validated", Message: "valid", ObservedGeneration: 2, LastTransitionTime: metav1.Time{Time: time.Unix(2, 0)}}}}
	if !publicCapabilityStatusEqual(a, b) {
		t.Fatal("timestamp-only change should not patch status")
	}
	b.Conditions[0].Reason = "InvalidDeclaration"
	if publicCapabilityStatusEqual(a, b) {
		t.Fatal("reason change must patch status")
	}
	if a.Conditions[0].LastTransitionTime.Time != time.Unix(1, 0) {
		t.Fatal("comparison mutated live status")
	}
}

func TestCapabilityEvidenceRequeueSchedulesFreshnessBoundary(t *testing.T) {
	now := time.Now().UTC()
	agent := &kyberv1.Agent{Spec: kyberv1.AgentSpec{PublicCapabilities: &kyberv1.AgentPublicCapabilities{Capabilities: []kyberv1.AgentPublicCapability{{Evidence: &kyberv1.AgentPublicCapabilityEvidence{RequiredSkills: []string{"review"}}}}}}}
	report := &skillscan.Report{ReportedAt: now.Format(time.RFC3339)}
	got := capabilityEvidenceRequeue(agent, report)
	if got < capabilities.SkillEvidenceMaxAge-time.Minute || got > capabilities.SkillEvidenceMaxAge+time.Minute {
		t.Fatalf("requeue=%s", got)
	}
}
