package chart

import (
	"strings"
	"testing"
)

func TestA2AFlagRendersAndReachesControlPlane(t *testing.T) {
	rendered := helmTemplate(t, "api.a2a.enabled=true", "api.durableTasks.retentionHours=72")
	if !strings.Contains(rendered, `KYBER_A2A_ENABLED: "true"`) {
		t.Fatal("control-plane ConfigMap does not render enabled A2A flag")
	}
	if !strings.Contains(rendered, `KYBER_TASKS_RETENTION_HOURS: "72"`) {
		t.Fatal("durable task retentionHours no longer renders from its documented parent")
	}
	container := container(t, findControlPlaneDeployment(t, rendered))
	envs, _ := container["env"].([]any)
	for _, item := range envs {
		env, _ := item.(map[string]any)
		if env["name"] != "KYBER_A2A_ENABLED" {
			continue
		}
		valueFrom, _ := env["valueFrom"].(map[string]any)
		ref, _ := valueFrom["configMapKeyRef"].(map[string]any)
		if ref["key"] != "KYBER_A2A_ENABLED" {
			t.Fatalf("A2A env references key %v", ref["key"])
		}
		return
	}
	t.Fatal("control-plane Deployment does not consume KYBER_A2A_ENABLED")
}
