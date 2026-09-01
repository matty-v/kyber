package chart

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestDurableTaskSettingsReachTheContainer is the regression guard for a bug
// that shipped in #199/#203: `api.durableTasks.enabled` rendered into the
// control-plane ConfigMap and was never injected into the container, so the
// process — which reads it with os.Getenv — never saw it and the feature could
// not be switched on from its own documented setting on any cluster.
//
// The same defect covered every durable-task LIMIT too, and it was invisible
// because each chart default happens to equal the compiled default in
// taskstore.DefaultLimits(). Nothing diverged until an operator changed one,
// at which point the change would have been silently ignored.
//
// The assertion is deliberately GENERAL rather than a check for the one key
// that broke: any KYBER_TASK* key the ConfigMap renders must be reachable by
// the control-plane container. A test naming individual keys would pass again
// the next time somebody adds a setting and forgets the wiring, which is the
// failure mode being guarded.
func TestDurableTaskSettingsReachTheContainer(t *testing.T) {
	rendered := helmTemplate(t)

	configured := taskKeysInControlPlaneConfigMap(t, rendered)
	if len(configured) == 0 {
		t.Fatal("no KYBER_TASK* keys found in the control-plane ConfigMap — has the chart moved?")
	}

	reachable := taskEnvKeysOnControlPlaneContainer(t, rendered)

	var missing []string
	for _, key := range configured {
		if !reachable[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d durable-task setting(s) are rendered into the ConfigMap but never reach the container, "+
			"so the process cannot read them:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestDurableTaskEnableFlagIsWired pins the specific key whose inertness was
// found on canary, so the fix has a named test and not only a general sweep.
func TestDurableTaskEnableFlagIsWired(t *testing.T) {
	rendered := helmTemplate(t, "api.durableTasks.enabled=true")

	if !taskEnvKeysOnControlPlaneContainer(t, rendered)["KYBER_TASKS_ENABLED"] {
		t.Fatal("KYBER_TASKS_ENABLED does not reach the control-plane container — durable tasks cannot be enabled")
	}
	// And the value the operator asked for is what the ConfigMap carries.
	if !regexp.MustCompile(`KYBER_TASKS_ENABLED:\s*"true"`).MatchString(rendered) {
		t.Error("api.durableTasks.enabled=true did not render KYBER_TASKS_ENABLED: \"true\"")
	}
}

// taskKeysInControlPlaneConfigMap returns the KYBER_TASK* data keys the
// control-plane ConfigMap renders.
func taskKeysInControlPlaneConfigMap(t *testing.T, rendered string) []string {
	t.Helper()
	var keys []string
	forEachDoc(t, rendered, func(doc map[string]any) {
		if kind, _ := doc["kind"].(string); kind != "ConfigMap" {
			return
		}
		if !isControlPlaneComponent(doc) {
			return
		}
		data, _ := doc["data"].(map[string]any)
		for key := range data {
			if strings.HasPrefix(key, "KYBER_TASK") {
				keys = append(keys, key)
			}
		}
	})
	return keys
}

// taskEnvKeysOnControlPlaneContainer returns the KYBER_TASK* env var names the
// control-plane container can actually read — whether named individually or
// pulled in wholesale via envFrom, since either wiring is a valid fix.
func taskEnvKeysOnControlPlaneContainer(t *testing.T, rendered string) map[string]bool {
	t.Helper()
	reachable := map[string]bool{}
	forEachDoc(t, rendered, func(doc map[string]any) {
		if kind, _ := doc["kind"].(string); kind != "Deployment" {
			return
		}
		if !isControlPlaneComponent(doc) {
			return
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		for _, raw := range containers {
			container, _ := raw.(map[string]any)
			if name, _ := container["name"].(string); name != "control-plane" {
				continue
			}
			env, _ := container["env"].([]any)
			for _, e := range env {
				entry, _ := e.(map[string]any)
				if name, _ := entry["name"].(string); strings.HasPrefix(name, "KYBER_TASK") {
					reachable[name] = true
				}
			}
			// envFrom on the control-plane ConfigMap makes every key in it
			// readable, so treat that as covering all of them.
			if envFrom, _ := container["envFrom"].([]any); len(envFrom) > 0 {
				for _, f := range envFrom {
					src, _ := f.(map[string]any)
					if ref, _ := src["configMapRef"].(map[string]any); ref != nil {
						for _, key := range taskKeysInControlPlaneConfigMap(t, rendered) {
							reachable[key] = true
						}
					}
				}
			}
		}
	})
	return reachable
}

func isControlPlaneComponent(doc map[string]any) bool {
	metadata, _ := doc["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	component, _ := labels["app.kubernetes.io/component"].(string)
	return component == "control-plane"
}

func forEachDoc(t *testing.T, rendered string, fn func(map[string]any)) {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			return
		}
		if doc != nil {
			fn(doc)
		}
	}
}
