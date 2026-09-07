package agent

import (
	"context"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
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

func TestRuntimeEvidenceExpiryPersistsPublicUnavailability(t *testing.T) {
	k8s, stop := setupEnvtest(t)
	defer stop()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime-evidence-expiry"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent("evidence", ns.Name)
	a.Spec.Runtime = "codex"
	a.Spec.RequestReplyEnabled = true
	a.Spec.PublicCapabilities = &kyberv1.AgentPublicCapabilities{
		SchemaVersion: capabilities.SchemaV1Alpha1,
		Identity:      kyberv1.AgentPublicCapabilityIdentity{DisplayName: "Fixture", Description: "Contract test"},
		Capabilities:  []kyberv1.AgentPublicCapability{{ID: "review", Version: "1", Name: "Review", Description: "Review code", InputModes: []string{"text/plain"}, OutputModes: []string{"text/plain"}, TaskFeatures: []string{"durable"}}},
	}
	if err := k8s.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	usable := true
	a.Status.Phase = kyberv1.AgentPhaseRunning
	a.Status.ObservedGeneration = a.Generation
	a.Status.Runtime.Runtime = "codex"
	a.Status.Runtime.Usable = &usable
	a.Status.Runtime.InstalledVersion = "test-version"
	a.Status.Runtime.Capabilities = &kyberv1.RuntimeCapabilitiesObservation{ContractVersion: runtimes.ContractVersion, InstalledVersion: "test-version", PodUID: "fixture", ObservedAt: metav1.Now(), Features: map[string]bool{"task-receipts": true, "task-tools": true}}
	if err := k8s.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	r := &AgentReconciler{Client: k8s, CapabilityPlatform: capabilities.PlatformState{TasksEnabled: true, DurableTaskStore: true}}
	wait, err := r.reconcilePublicCapabilities(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if wait <= 0 || wait > runtimes.ObservationTTL+time.Second {
		t.Fatalf("runtime-only evidence expiry not scheduled: %s", wait)
	}
	if got := a.Status.PublicCapabilities.Capabilities[0].Availability; got != "available" {
		t.Fatalf("fresh evidence: %s (%+v)", got, a.Status.PublicCapabilities)
	}
	// Advance the persisted sample past the boundary instead of sleeping 90s.
	a.Status.Runtime.Capabilities.ObservedAt = metav1.NewTime(time.Now().Add(-runtimes.ObservationTTL - time.Second))
	if err := k8s.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcilePublicCapabilities(ctx, a); err != nil {
		t.Fatal(err)
	}
	got := &kyberv1.Agent{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: a.Name, Namespace: a.Namespace}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.PublicCapabilities.Capabilities[0].Availability != "unknown" {
		t.Fatalf("expired evidence still advertised: %+v", got.Status.PublicCapabilities)
	}
}

func TestCapabilityExpiryUsesEarliestEvidence(t *testing.T) {
	a := &kyberv1.Agent{Spec: kyberv1.AgentSpec{PublicCapabilities: &kyberv1.AgentPublicCapabilities{Capabilities: []kyberv1.AgentPublicCapability{{TaskFeatures: []string{"durable"}, Evidence: &kyberv1.AgentPublicCapabilityEvidence{RequiredSkills: []string{"review"}}}}}}}
	a.Status.Runtime.Capabilities = &kyberv1.RuntimeCapabilitiesObservation{ObservedAt: metav1.Now()}
	for _, report := range []*skillscan.Report{nil, {ReportedAt: "invalid"}, {ReportedAt: time.Now().Format(time.RFC3339)}} {
		wait := capabilityEvidenceRequeue(a, report)
		if wait < 80*time.Second || wait > 91*time.Second {
			t.Fatalf("runtime expiry masked by skill report: %s", wait)
		}
	}
}
