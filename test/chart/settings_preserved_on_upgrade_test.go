// Tests for the "operator-written settings survive a helm upgrade" contract.
//
// Background: two chart-templated resources hold state an operator writes at
// RUNTIME through the PWA — the fleet-defaults ConfigMap
// (PUT /api/v1/fleet-defaults) and the Anthropic key Secret
// (PUT /api/v1/settings/anthropic-key). Both used to render straight from
// `.Values`, so any real `helm upgrade` reverted them to the chart seed. For
// the Secret that meant writing an EMPTY api-key over the operator's, silently
// breaking model discovery fleet-wide.
//
// That never fired because nothing ever ran `helm upgrade` against Kyber:
// ArgoCD renders the chart and applies manifests, and per-cluster
// `ignoreDifferences` plus the argocd-* annotations covered the drift. Making
// Kyber upgrade ITSELF via Helm (dave-agent spec
// 2026-08-10-kyber-owns-its-deployment.md) arms the bug, and an automatic
// overnight upgrade would have been the first thing to pull the trigger.
//
// The fix is a `lookup` of the live resource, with LIVE-WINS precedence: chart
// values seed a first install, the live value wins thereafter. Live-wins rather
// than values-win because `defaultModel` and `codexDefaultModel` ship non-empty
// defaults, and Helm cannot tell "the operator overrode this" from "this is the
// chart default" — a values-first rule would clobber edits to exactly the two
// fields most likely to be edited.
//
// SCOPE OF THESE TESTS — read before trusting them. `helm template` has no
// cluster access, so `lookup` always returns empty here and the LIVE tier
// cannot be exercised by a unit test. What follows therefore pins:
//
//   - the seed path still renders correctly (no regression for fresh installs);
//   - the templates still parse and render with lookup returning nothing;
//   - the render is not accidentally emitting an empty value where a seed exists.
//
// The preserve-across-upgrade behaviour itself needs a real API server and is
// verified in the e2e suite, which installs into a live k3d cluster. A green
// run here does NOT prove preservation.
package chart

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// findDoc pulls the first rendered document matching kind+name out of a
// multi-doc manifest stream.
func findDoc(t *testing.T, rendered, kind, name string) map[string]any {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---\n") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil || m == nil {
			continue
		}
		if m["kind"] != kind {
			continue
		}
		meta, _ := m["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		if n, _ := meta["name"].(string); n == name {
			return m
		}
	}
	t.Fatalf("no %s named %q in rendered output", kind, name)
	return nil
}

func dataString(t *testing.T, doc map[string]any, field, key string) string {
	t.Helper()
	d, ok := doc[field].(map[string]any)
	if !ok {
		t.Fatalf("%s has no %s block", doc["kind"], field)
	}
	v, ok := d[key]
	if !ok {
		t.Fatalf("%s.%s has no key %q", doc["kind"], field, key)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s.%s[%q] is %T, want string", doc["kind"], field, key, v)
	}
	return s
}

// The seed path: with no live cluster (lookup empty), chart values must still
// reach the ConfigMap. This is the fresh-install case and the one behaviour a
// unit test can actually assert.
func TestFleetDefaults_SeedsFromChartValuesWhenNoLiveValue(t *testing.T) {
	rendered := helmTemplate(t,
		"controlPlane.fleetDefaults.defaultModel=claude-seed-model",
		"controlPlane.fleetDefaults.codexDefaultModel=codex-seed-model",
	)
	cm := findDoc(t, rendered, "ConfigMap", "kyber-fleet-defaults")

	if got := dataString(t, cm, "data", "defaultModel"); got != "claude-seed-model" {
		t.Errorf("defaultModel = %q, want the chart seed %q", got, "claude-seed-model")
	}
	if got := dataString(t, cm, "data", "codexDefaultModel"); got != "codex-seed-model" {
		t.Errorf("codexDefaultModel = %q, want the chart seed %q", got, "codex-seed-model")
	}
}

// Fresh installs follow upstream harness releases rather than pinning the
// version baked into the image.
func TestFleetDefaults_RuntimeVersionDefaultsToLatest(t *testing.T) {
	rendered := helmTemplate(t, "runtime.claudeCode.version=9.9.9")
	cm := findDoc(t, rendered, "ConfigMap", "kyber-fleet-defaults")

	if got := dataString(t, cm, "data", "defaultRuntimeVersion"); got != "latest" {
		t.Errorf("defaultRuntimeVersion = %q, want latest", got)
	}
}

// An explicit chart value must still beat the derived fallback on a fresh
// install — the lookup change must not have inverted these two.
func TestFleetDefaults_ExplicitValueBeatsDerivedFallback(t *testing.T) {
	rendered := helmTemplate(t,
		"runtime.claudeCode.version=9.9.9",
		"controlPlane.fleetDefaults.defaultRuntimeVersion=1.2.3",
	)
	cm := findDoc(t, rendered, "ConfigMap", "kyber-fleet-defaults")

	if got := dataString(t, cm, "data", "defaultRuntimeVersion"); got != "1.2.3" {
		t.Errorf("defaultRuntimeVersion = %q, want the explicit chart value %q", got, "1.2.3")
	}
}

// The Anthropic key seeds from chart values when there is no live Secret. The
// value is empty by default (the PWA is the canonical write path), so this
// asserts the wiring rather than a particular key.
func TestAnthropicKey_SeedsFromChartValueWhenNoLiveSecret(t *testing.T) {
	rendered := helmTemplate(t, "runtimeDetect.anthropicApiKey=sk-seed-value")
	sec := findDoc(t, rendered, "Secret", "kyber-anthropic-key")

	if got := dataString(t, sec, "stringData", "api-key"); got != "sk-seed-value" {
		t.Errorf("api-key = %q, want the chart seed %q", got, "sk-seed-value")
	}
}

// With nothing set anywhere the key renders empty — the fresh-install state.
// Pinned so a future refactor cannot make the template render something
// non-empty (a placeholder, a literal "null") that the poller would then try
// to authenticate with.
func TestAnthropicKey_EmptyWhenUnset(t *testing.T) {
	rendered := helmTemplate(t)
	sec := findDoc(t, rendered, "Secret", "kyber-anthropic-key")

	if got := dataString(t, sec, "stringData", "api-key"); got != "" {
		t.Errorf("api-key = %q, want empty on a fresh install with nothing configured", got)
	}
}

// The ArgoCD drift annotations must stay while any cluster is still
// ArgoCD-managed.
//
// CORRECTED after review: these annotations are NOT what protects field-level
// drift. `IgnoreExtraneous` covers extraneous RESOURCES, not fields, and
// `Replace=false` is already the default — kyber-deploy's razer manifest says
// so explicitly. The real protection is the per-cluster `ignoreDifferences` on
// /data in each deploy repo. These are kept as defence in depth and because
// removing them is churn, but do not read this test as proof that an ArgoCD
// cluster is safe without its ignoreDifferences entry — it is not.
//
// Delete this test in the same change that retires the last ArgoCD cluster.
func TestFleetDefaults_ArgoCDDriftAnnotationsRetainedDuringMigration(t *testing.T) {
	rendered := helmTemplate(t)
	cm := findDoc(t, rendered, "ConfigMap", "kyber-fleet-defaults")

	meta, _ := cm["metadata"].(map[string]any)
	anns, ok := meta["annotations"].(map[string]any)
	if !ok {
		t.Fatal("fleet-defaults ConfigMap has no annotations block")
	}
	for _, want := range []string{
		"argocd.argoproj.io/compare-options",
		"argocd.argoproj.io/sync-options",
	} {
		if _, present := anns[want]; !present {
			t.Errorf("annotation %q missing; keep it while any cluster is still ArgoCD-managed (note: the real field-drift protection is the per-cluster ignoreDifferences on /data, not this annotation)", want)
		}
	}
}

func TestFleetDefaults_RuntimeVersionsIgnoreImagePins(t *testing.T) {
	rendered := helmTemplate(t,
		"runtime.claudeCode.version=9.9.9",
		"runtime.codex.version=8.8.8",
	)
	cm := findDoc(t, rendered, "ConfigMap", "kyber-fleet-defaults")

	if got := dataString(t, cm, "data", "defaultRuntimeVersion"); got != "latest" {
		t.Errorf("defaultRuntimeVersion = %q, want latest", got)
	}
	if got := dataString(t, cm, "data", "codexDefaultRuntimeVersion"); got != "latest" {
		t.Errorf("codexDefaultRuntimeVersion = %q, want latest", got)
	}
}
