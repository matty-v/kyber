package chart

import (
	"strings"
	"testing"
)

// TestModelContextWindows_EnabledRendersConfigMapAndEnv asserts kyber#378
// PR-D wiring is intact when runtimeDetect.enabled=true (the default):
//
//   - the kyber-model-context-windows ConfigMap is rendered with the
//     seeded model→tokens map
//   - the control-plane Deployment carries
//     KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP so pkg/contextwindowmap.Resolver
//     loads the same ConfigMap on boot
//
// Missing either piece silently downgrades every model to the 200K floor
// — the failure mode this chart test guards.
func TestModelContextWindows_EnabledRendersConfigMapAndEnv(t *testing.T) {
	rendered := helmTemplate(t)

	if !strings.Contains(rendered, "name: kyber-model-context-windows") {
		t.Errorf("expected kyber-model-context-windows ConfigMap in rendered chart")
	}
	// Seed entries from values.yaml's defaults — assert at least one
	// well-known 1M model lands so a future values change that breaks
	// the wiring fails this test loudly.
	if !strings.Contains(rendered, `"claude-opus-4-7"`) || !strings.Contains(rendered, `"1000000"`) {
		t.Errorf("expected claude-opus-4-7 → 1000000 seed in rendered ConfigMap")
	}

	deploy := findControlPlaneDeployment(t, rendered)
	c := container(t, deploy)
	env := envNames(c)
	if v, ok := env["KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP"]; !ok || v != "kyber-model-context-windows" {
		t.Errorf("KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP env: got %q, want kyber-model-context-windows", v)
	}
}

// TestModelContextWindows_DisabledOmitsAllWiring guards the air-gapped
// install posture: runtimeDetect.enabled=false must omit the ConfigMap
// AND the env var so the resolver runs in disabled mode (every model
// falls back to the 200K floor without crashing on a missing ConfigMap).
func TestModelContextWindows_DisabledOmitsAllWiring(t *testing.T) {
	rendered := helmTemplate(t, "runtimeDetect.enabled=false")

	if strings.Contains(rendered, "name: kyber-model-context-windows") {
		t.Errorf("kyber-model-context-windows ConfigMap should not render when runtimeDetect.enabled=false")
	}
	deploy := findControlPlaneDeployment(t, rendered)
	c := container(t, deploy)
	if _, present := envNames(c)["KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP"]; present {
		t.Errorf("KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP env should be omitted when runtimeDetect.enabled=false")
	}
}

// TestModelContextWindows_EmptyConfigMapNameOmitsWiring: keeping
// runtimeDetect.enabled=true but blanking the ConfigMap name is the
// "use the in-Go knownModels table only" escape hatch. Both the
// ConfigMap and the env var must drop out so the resolver short-
// circuits to the floor for every unmapped model.
func TestModelContextWindows_EmptyConfigMapNameOmitsWiring(t *testing.T) {
	rendered := helmTemplate(t, "runtimeDetect.contextWindowsConfigMapName=")

	if strings.Contains(rendered, "name: kyber-model-context-windows") {
		t.Errorf("ConfigMap should not render when contextWindowsConfigMapName is empty")
	}
	deploy := findControlPlaneDeployment(t, rendered)
	c := container(t, deploy)
	if _, present := envNames(c)["KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP"]; present {
		t.Errorf("KYBER_MODEL_CONTEXT_WINDOWS_CONFIGMAP env should be omitted when contextWindowsConfigMapName is empty")
	}
}
