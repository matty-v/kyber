// Tests for the image-tag fail-loud contract (kyber#358 follow-up).
//
// Background: the control-plane Deployment template used to render image
// references as `repo:{{ .tag | default .Chart.AppVersion }}`. When an image
// tag was not pinned (e.g. ArgoCD Image Updater's parameter override was lost
// on an app refresh), the tag fell back to Chart.AppVersion ("0.1.0") — a
// placeholder that was never published to GHCR. The resulting `:0.1.0`
// reference is a 404, and the sidecar-image convergence loop (kyber#358, the
// unconditional 5d roll in the agent reconciler) then deleted every running
// agent pod to "converge" it onto the phantom image, killing agents
// mid-session. See the R2-D2 crash investigation 2026-05-29.
//
// The fix makes image tags REQUIRED at render time: a missing pin fails the
// `helm template`/ArgoCD sync loudly instead of silently emitting a 404 that
// gets enforced fleet-wide.
//
// kyber#457 update: Chart.AppVersion now tracks the real release version (it is
// no longer the unpublished "0.1.0" placeholder). That makes the `required`
// guards MORE important, not less — an accidental AppVersion fallback would now
// render a real-looking `repo:<release>` tag and resolve silently instead of
// 404'ing. The fallback guard below therefore keys off the live appVersion, not
// a hardcoded "0.1.0".
package chart

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// chartAppVersion reads the chart's appVersion from Chart.yaml. The image-tag
// fallback guard below must key off the LIVE appVersion, not a hardcoded
// literal: once the release pipeline advances appVersion to a real published
// tag (kyber#457), a hardcoded ":0.1.0" check would silently stop catching an
// AppVersion image fallback (it would now render ":1.8.0", a real-looking tag).
func chartAppVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(chartDir(t), "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	var meta struct {
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse Chart.yaml: %v", err)
	}
	if meta.AppVersion == "" {
		t.Fatal("Chart.yaml appVersion is empty")
	}
	return meta.AppVersion
}

// helmTemplateExpectError runs `helm template` with the base credential flags
// plus any extra --set args, and returns (stdout, stderr, err). Unlike
// helmTemplate it does NOT pin image tags, so callers can exercise the
// missing-tag path. Skips when helm is unavailable.
func helmTemplateExpectError(t *testing.T, extraSet ...string) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not on PATH: %v", err)
	}
	args := []string{
		"template", "kyber", chartDir(t),
		"--set", "api.apiKey=test123",
		"--set", "api.webhookSecret=webhook123",
		"--set", "k3s.joinToken=K10abc",
		"--set", "k3s.serverUrl=https://10.0.0.1:6443",
	}
	args = append(args, extraSet...)
	cmd := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// TestImageTag_MissingPinFailsLoudly asserts that rendering the chart without a
// pinned status-sidecar image tag FAILS, rather than silently falling back to
// Chart.AppVersion and emitting an unpullable `:0.1.0` reference.
func TestImageTag_MissingPinFailsLoudly(t *testing.T) {
	// Pin every image tag EXCEPT statusSidecar, so we isolate the sidecar tag
	// as the cause of the failure.
	_, stderr, err := helmTemplateExpectError(t,
		"--set", "image.controlPlane.tag=test",
		"--set", "image.nodeAgent.tag=test",
		"--set", "image.claudeCode.tag=test",
		// image.statusSidecar.tag deliberately left unset
	)
	if err == nil {
		t.Fatalf("expected `helm template` to fail when image.statusSidecar.tag is unset, but it succeeded")
	}
	if !strings.Contains(stderr, "statusSidecar.tag") {
		t.Errorf("expected error to name image.statusSidecar.tag; got stderr:\n%s", stderr)
	}
}

// TestImageTag_RuntimeImageMissingPinFailsLoudly is the same contract for the
// agent runtime image (KYBER_AGENT_RUNTIME_IMAGE / image.claudeCode.tag).
func TestImageTag_RuntimeImageMissingPinFailsLoudly(t *testing.T) {
	_, stderr, err := helmTemplateExpectError(t,
		"--set", "image.controlPlane.tag=test",
		"--set", "image.nodeAgent.tag=test",
		"--set", "image.statusSidecar.tag=test",
		// image.claudeCode.tag deliberately left unset
	)
	if err == nil {
		t.Fatalf("expected `helm template` to fail when image.claudeCode.tag is unset, but it succeeded")
	}
	if !strings.Contains(stderr, "claudeCode.tag") {
		t.Errorf("expected error to name image.claudeCode.tag; got stderr:\n%s", stderr)
	}
}

// TestImageTag_PinnedRendersExpectedRef asserts the positive path: when tags
// are pinned, the control-plane env carries exactly the pinned reference (no
// AppVersion substitution).
func TestImageTag_PinnedRendersExpectedRef(t *testing.T) {
	rendered := helmTemplate(t,
		"image.statusSidecar.tag=latest@sha256:deadbeef",
		"image.claudeCode.tag=latest@sha256:cafef00d",
	)
	deploy := findControlPlaneDeployment(t, rendered)
	env := envNames(container(t, deploy))

	if got := env["KYBER_STATUS_SIDECAR_IMAGE"]; !strings.HasSuffix(got, ":latest@sha256:deadbeef") {
		t.Errorf("KYBER_STATUS_SIDECAR_IMAGE = %q, want suffix :latest@sha256:deadbeef", got)
	}
	if got := env["KYBER_AGENT_RUNTIME_IMAGE"]; !strings.HasSuffix(got, ":latest@sha256:cafef00d") {
		t.Errorf("KYBER_AGENT_RUNTIME_IMAGE = %q, want suffix :latest@sha256:cafef00d", got)
	}
	// Guard against the regression: Chart.AppVersion must never leak into an image
	// ref. Keyed off the LIVE appVersion (not a ":0.1.0" literal) so the guard
	// keeps biting after the release pipeline advances appVersion to a real tag
	// (kyber#457) — a fallback would then render `repo:<appVersion>`, which looks
	// real and would resolve SILENTLY rather than 404'ing loudly.
	appVer := chartAppVersion(t)
	fallback := ":" + appVer
	if strings.HasSuffix(env["KYBER_STATUS_SIDECAR_IMAGE"], fallback) ||
		strings.HasSuffix(env["KYBER_AGENT_RUNTIME_IMAGE"], fallback) {
		t.Errorf("image ref fell back to Chart.AppVersion (%s); sidecar=%q runtime=%q",
			fallback, env["KYBER_STATUS_SIDECAR_IMAGE"], env["KYBER_AGENT_RUNTIME_IMAGE"])
	}
}
