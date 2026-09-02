package capabilities

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/skillscan"
)

func validDeclaration() *kyberv1.AgentPublicCapabilities {
	return &kyberv1.AgentPublicCapabilities{
		SchemaVersion: SchemaV1Alpha1,
		Identity:      kyberv1.AgentPublicCapabilityIdentity{DisplayName: "Deployment assistant", Description: "Plans bounded deployments.", DocumentationURL: "https://docs.example.com/deployer"},
		Capabilities: []kyberv1.AgentPublicCapability{{
			ID: "deployment-plan", Version: "1", Name: "Plan a deployment", Description: "Produces a reviewed plan.",
			InputModes: []string{"application/json", "text/plain"}, OutputModes: []string{"text/markdown"},
			TaskFeatures: []string{"typed-results", "durable"},
			Evidence:     &kyberv1.AgentPublicCapabilityEvidence{RequiredSkills: []string{"internal-skill-42"}, RuntimeAdapters: []string{"codex"}},
		}},
	}
}

func TestNormalizeAndValidateStableAndPrivate(t *testing.T) {
	declared := validDeclaration()
	first, firstDigest, err := NormalizeAndValidate(declared)
	if err != nil {
		t.Fatal(err)
	}
	declared.Capabilities[0].InputModes = []string{"text/plain", "application/json"}
	second, secondDigest, err := NormalizeAndValidate(declared)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("digest changed under order-only edit: %s != %s", firstDigest, secondDigest)
	}
	encoded, _ := json.Marshal(first)
	for _, private := range []string{"evidence", "internal-skill-42", "codex", "path", "prompt", "model"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("public projection leaked %q: %s", private, encoded)
		}
	}
	if len(second.Capabilities) != 1 {
		t.Fatalf("capabilities = %d", len(second.Capabilities))
	}
}

func TestNormalizeAndValidateRejectsUnsafeDeclarations(t *testing.T) {
	tests := map[string]func(*kyberv1.AgentPublicCapabilities){
		"duplicate id":    func(m *kyberv1.AgentPublicCapabilities) { m.Capabilities = append(m.Capabilities, m.Capabilities[0]) },
		"unknown feature": func(m *kyberv1.AgentPublicCapabilities) { m.Capabilities[0].TaskFeatures = []string{"telepathy"} },
		"parameterized mime": func(m *kyberv1.AgentPublicCapabilities) {
			m.Capabilities[0].InputModes = []string{"text/plain; charset=utf-8"}
		},
		"private url": func(m *kyberv1.AgentPublicCapabilities) { m.Identity.DocumentationURL = "https://127.0.0.1/private" },
		"service url": func(m *kyberv1.AgentPublicCapabilities) {
			m.Identity.DocumentationURL = "https://grafana.monitoring.svc/dashboard"
		},
		"cluster url": func(m *kyberv1.AgentPublicCapabilities) {
			m.Identity.DocumentationURL = "https://grafana.monitoring.svc.cluster.local/dashboard"
		},
		"markup":        func(m *kyberv1.AgentPublicCapabilities) { m.Capabilities[0].Description = "<script>alert(1)</script>" },
		"secret shaped": func(m *kyberv1.AgentPublicCapabilities) { m.Identity.Description = "-----BEGIN PRIVATE KEY-----" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m := validDeclaration()
			mutate(m)
			if _, _, err := NormalizeAndValidate(m); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestEvaluateRequiresInstallationTaskBackends(t *testing.T) {
	now := time.Date(2026, 9, 1, 22, 0, 0, 0, time.UTC)
	usable := true
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Spec: kyberv1.AgentSpec{Runtime: "codex", RequestReplyEnabled: true, PublicCapabilities: validDeclaration()}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning, Runtime: kyberv1.AgentRuntimeStatus{Runtime: "codex", Usable: &usable}}}
	agent.Spec.PublicCapabilities.Capabilities[0].TaskFeatures = append(agent.Spec.PublicCapabilities.Capabilities[0].TaskFeatures, "files")
	report := &skillscan.Report{ReportedAt: now.Format(time.RFC3339), Skills: []skillscan.Skill{{Name: "internal-skill-42", Linked: []string{"codex"}}}}

	tests := []struct {
		name     string
		platform PlatformState
		reason   string
	}{
		{"tasks disabled", PlatformState{}, "platform-task-service-disabled"},
		{"store missing", PlatformState{TasksEnabled: true}, "platform-task-store-unavailable"},
		{"object store missing", PlatformState{TasksEnabled: true, DurableTaskStore: true}, "platform-object-store-unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := Evaluate(agent, report, nil, tc.platform, now)
			if got := status.Capabilities[0]; got.Availability != "unavailable" || got.Reason != tc.reason {
				t.Fatalf("availability = %+v, want unavailable/%s", got, tc.reason)
			}
		})
	}
	status := Evaluate(agent, report, nil, PlatformState{TasksEnabled: true, DurableTaskStore: true, TaskObjectStore: true}, now)
	if got := status.Capabilities[0]; got.Availability != "available" {
		t.Fatalf("all backends ready = %+v", got)
	}
}

func TestEvaluateFailsClosedAndRecovers(t *testing.T) {
	now := time.Date(2026, 9, 1, 22, 0, 0, 0, time.UTC)
	usable := true
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Spec: kyberv1.AgentSpec{Runtime: "codex", RequestReplyEnabled: true, PublicCapabilities: validDeclaration()}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning, Runtime: kyberv1.AgentRuntimeStatus{Runtime: "codex", Usable: &usable}}}
	platform := PlatformState{TasksEnabled: true, DurableTaskStore: true, TaskObjectStore: true}
	status := Evaluate(agent, nil, errors.New("down"), platform, now)
	if got := status.Capabilities[0]; got.Availability != "unknown" || got.Reason != "skill-evidence-unavailable" {
		t.Fatalf("unavailable evidence = %+v", got)
	}
	report := &skillscan.Report{ReportedAt: now.Format(time.RFC3339), Skills: []skillscan.Skill{{Name: "internal-skill-42", Linked: []string{"codex"}}}}
	status = Evaluate(agent, report, nil, platform, now)
	if got := status.Capabilities[0]; got.Availability != "available" {
		t.Fatalf("healthy evidence = %+v", got)
	}
	report.ReportedAt = now.Add(-SkillEvidenceMaxAge - time.Second).Format(time.RFC3339)
	status = Evaluate(agent, report, nil, platform, now)
	if got := status.Capabilities[0]; got.Reason != "skill-evidence-stale" {
		t.Fatalf("stale evidence = %+v", got)
	}
	report.ReportedAt = now.Format(time.RFC3339)
	report.Skills[0].Linked = []string{"claude-code"}
	status = Evaluate(agent, report, nil, platform, now)
	if got := status.Capabilities[0]; got.Reason != "required-skill-unlinked" {
		t.Fatalf("wrong runtime = %+v", got)
	}
	agent.Spec.PublicCapabilities.Capabilities[0].Evidence.RequiredConnectors = []string{"github"}
	report.Skills[0].Linked = []string{"codex"}
	status = Evaluate(agent, report, nil, platform, now)
	if got := status.Capabilities[0]; got.Reason != "required-connector-missing" {
		t.Fatalf("missing connector = %+v", got)
	}
}
