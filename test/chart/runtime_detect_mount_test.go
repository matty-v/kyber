package chart

import (
	"strings"
	"testing"
)

// TestRuntimeDetect_EnabledRendersSecretAndMount asserts the
// runtime-detection wiring is intact when runtimeDetect.enabled=true (the
// default). The poller relies on:
//
//   - KYBER_RUNTIMEDETECT_* env vars being present on the control-plane
//     container (so the goroutine boots with the configured cadence + limit)
//   - the anthropic-key Secret volume being mounted at
//     /etc/kyber-anthropic-key (so FileKeySource can pick up the operator-
//     entered key, and rotations propagate without a pod restart)
//
// A missing env var or missing mount would silently disable detection at
// runtime — exactly the failure mode this chart test guards.
func TestRuntimeDetect_EnabledRendersSecretAndMount(t *testing.T) {
	rendered := helmTemplate(t)

	// Anthropic-key Secret rendered.
	if !strings.Contains(rendered, "name: kyber-anthropic-key") {
		t.Errorf("expected anthropic-key Secret in rendered chart")
	}

	deploy := findControlPlaneDeployment(t, rendered)
	c := container(t, deploy)
	env := envNames(c)
	for _, want := range []string{
		"KYBER_RUNTIMEDETECT_ENABLED",
		"KYBER_RUNTIMEDETECT_CADENCE_SECONDS",
		"KYBER_RUNTIMEDETECT_VERSION_LIMIT",
		"KYBER_ANTHROPIC_KEY_SECRET_NAME",
		"KYBER_ANTHROPIC_KEY_PATH",
	} {
		if _, ok := env[want]; !ok {
			t.Errorf("missing env var %s in control-plane container", want)
		}
	}

	// Volume + mount wired through.
	mounts, _ := c["volumeMounts"].([]any)
	found := false
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm["name"] == "anthropic-key" && mm["mountPath"] == "/etc/kyber-anthropic-key" {
			found = true
			if ro, _ := mm["readOnly"].(bool); !ro {
				t.Errorf("anthropic-key volumeMount must be readOnly")
			}
		}
	}
	if !found {
		t.Errorf("expected anthropic-key volumeMount at /etc/kyber-anthropic-key")
	}

	ps := podSpec(t, deploy)
	volumes, _ := ps["volumes"].([]any)
	volFound := false
	for _, v := range volumes {
		vv, _ := v.(map[string]any)
		if vv["name"] == "anthropic-key" {
			volFound = true
			secret, _ := vv["secret"].(map[string]any)
			if secret["secretName"] != "kyber-anthropic-key" {
				t.Errorf("anthropic-key volume must reference kyber-anthropic-key Secret, got %v", secret["secretName"])
			}
			if opt, _ := secret["optional"].(bool); !opt {
				t.Errorf("anthropic-key Secret volume must be optional (pod boots before key is entered)")
			}
		}
	}
	if !volFound {
		t.Errorf("expected anthropic-key volume on pod spec")
	}
}

// TestRuntimeDetect_DisabledOmitsAllWiring guards the air-gapped install
// posture: when runtimeDetect.enabled=false, the Secret, env vars, and
// mount must all be omitted so an operator's `helm install` doesn't fail
// trying to project a Secret they didn't ask for.
func TestRuntimeDetect_DisabledOmitsAllWiring(t *testing.T) {
	rendered := helmTemplate(t, "runtimeDetect.enabled=false")

	if strings.Contains(rendered, "name: kyber-anthropic-key") {
		t.Errorf("anthropic-key Secret should not render when runtimeDetect.enabled=false")
	}
	deploy := findControlPlaneDeployment(t, rendered)
	c := container(t, deploy)
	env := envNames(c)
	if v, ok := env["KYBER_RUNTIMEDETECT_ENABLED"]; ok && v != "" {
		// The disabled branch still emits KYBER_RUNTIMEDETECT_ENABLED="false"
		// so the control-plane logs the explicit disable on boot. The
		// CADENCE/LIMIT/PATH vars must be absent.
	}
	for _, gone := range []string{
		"KYBER_RUNTIMEDETECT_CADENCE_SECONDS",
		"KYBER_RUNTIMEDETECT_VERSION_LIMIT",
		"KYBER_ANTHROPIC_KEY_SECRET_NAME",
		"KYBER_ANTHROPIC_KEY_PATH",
	} {
		if _, present := env[gone]; present {
			t.Errorf("env var %s should be omitted when runtimeDetect.enabled=false", gone)
		}
	}
	mounts, _ := c["volumeMounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm["name"] == "anthropic-key" {
			t.Errorf("anthropic-key volumeMount should be omitted when runtimeDetect.enabled=false")
		}
	}
}
