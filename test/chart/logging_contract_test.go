package chart

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func workloadTemplates(t *testing.T, rendered string) []map[string]any {
	t.Helper()
	var templates []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		if template != nil {
			templates = append(templates, template)
		}
	}
	return templates
}

func TestLoggingLabelsCoverRenderedWorkloads(t *testing.T) {
	rendered := helmTemplate(t,
		"logShipper.enabled=true", "logShipper.bucket=test-logs",
		"transcriptCompaction.enabled=true",
		"minio.enabled=true", "minio.rootPassword=test-password",
		"cloudflared.enabled=true", "cloudflared.tunnelId=test-tunnel",
		"cloudflared.existingSecret=test-cloudflared",
	)
	templates := workloadTemplates(t, rendered)
	if len(templates) == 0 {
		t.Fatal("no rendered pod templates")
	}
	seen := map[string]bool{}
	for _, template := range templates {
		metadata, _ := template["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		component, _ := labels["app.kubernetes.io/component"].(string)
		if component == "" {
			t.Errorf("pod template has no component label: %v", labels)
			continue
		}
		seen[component] = true
		if got := labels["app.kubernetes.io/part-of"]; got != "kyber" {
			t.Errorf("%s part-of label = %v, want kyber", component, got)
		}
	}
	for _, component := range []string{
		"control-plane", "node-agent", "postgres", "redis", "log-shipper",
		"transcript-compact", "minio", "minio-bootstrap", "cloudflared",
	} {
		if !seen[component] {
			t.Errorf("component %q was not rendered", component)
		}
	}
}

func TestLoggingLevelsAndDownwardContext(t *testing.T) {
	rendered := helmTemplate(t,
		"logging.level=warn",
		"logging.components.control-plane.level=debug",
		"logging.components.node-agent.level=error",
	)
	wantLevels := map[string]string{"control-plane": "debug", "node-agent": "error"}
	for _, template := range workloadTemplates(t, rendered) {
		metadata, _ := template["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		component, _ := labels["app.kubernetes.io/component"].(string)
		want, ok := wantLevels[component]
		if !ok {
			continue
		}
		spec, _ := template["spec"].(map[string]any)
		containers, _ := spec["containers"].([]any)
		container, _ := containers[0].(map[string]any)
		env := envNames(container)
		if got := env["KYBER_LOG_LEVEL"]; got != want {
			t.Errorf("%s KYBER_LOG_LEVEL = %q, want %q", component, got, want)
		}
		for _, name := range []string{"KYBER_LOG_POD", "KYBER_LOG_NAMESPACE", "KYBER_LOG_CONTAINER"} {
			if _, ok := env[name]; !ok {
				t.Errorf("%s missing %s", component, name)
			}
		}
	}
}

func TestLoggingRejectsInvalidLevel(t *testing.T) {
	_, stderr, err := helmTemplateExpectError(t,
		"--set", "image.controlPlane.tag=test",
		"--set", "image.nodeAgent.tag=test",
		"--set", "image.statusSidecar.tag=test",
		"--set", "image.claudeCode.tag=test",
		"--set", "logging.level=verbose",
	)
	if err == nil {
		t.Fatal("expected invalid logging level to fail rendering")
	}
	if !strings.Contains(stderr, "invalid logging level") {
		t.Errorf("error does not identify invalid logging level:\n%s", stderr)
	}
}
