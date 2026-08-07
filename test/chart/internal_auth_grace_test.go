// Tests for the kyber#578 GRACE-FIRST cutover default.
//
// After the v2.1.0 incident (the #566 enforce deploy shipped fail-closed but the
// out-of-band signing-key Secret was never delivered → the whole internal API
// 503'd → fleet outage), a NEW internal-auth rollout must default to grace so the
// cutover is a verified two-step sequence, not a single flip that fail-closes if
// the key is missing. These tests pin that the chart renders
// KYBER_INTERNAL_AUTH_GRACE=true by default, and that an explicit graceMode pin
// (the existing enforce clusters) is honored unchanged (back-compat).
package chart

import (
	"strings"
	"testing"
)

// TestInternalAuthGrace_DefaultsToGraceFirst asserts the chart-default behavior
// change: a fresh rollout that does not set internalAuth.graceMode renders the
// control-plane env KYBER_INTERNAL_AUTH_GRACE=true (grace-first, kyber#578).
func TestInternalAuthGrace_DefaultsToGraceFirst(t *testing.T) {
	rendered := helmTemplate(t,
		"image.statusSidecar.tag=test",
		"image.claudeCode.tag=test",
		"image.controlPlane.tag=test",
		"image.nodeAgent.tag=test",
	)
	deploy := findControlPlaneDeployment(t, rendered)
	env := envNames(container(t, deploy))

	got, ok := env["KYBER_INTERNAL_AUTH_GRACE"]
	if !ok {
		t.Fatal("control-plane env must carry KYBER_INTERNAL_AUTH_GRACE")
	}
	if strings.Trim(got, `"`) != "true" {
		t.Errorf("KYBER_INTERNAL_AUTH_GRACE default = %q, want \"true\" (grace-first, kyber#578)", got)
	}
}

// TestInternalAuthGrace_ExplicitEnforceHonored is the back-compat guard: a deploy
// that pins internalAuth.graceMode=false (the existing enforce clusters running
// #566) still renders enforce — the grace-first default only affects unset/fresh
// rollouts, never a deploy that set the value explicitly.
func TestInternalAuthGrace_ExplicitEnforceHonored(t *testing.T) {
	rendered := helmTemplate(t,
		"image.statusSidecar.tag=test",
		"image.claudeCode.tag=test",
		"image.controlPlane.tag=test",
		"image.nodeAgent.tag=test",
		"internalAuth.graceMode=false",
	)
	deploy := findControlPlaneDeployment(t, rendered)
	env := envNames(container(t, deploy))

	if got := strings.Trim(env["KYBER_INTERNAL_AUTH_GRACE"], `"`); got != "false" {
		t.Errorf("explicit graceMode=false must render KYBER_INTERNAL_AUTH_GRACE=false (back-compat); got %q", got)
	}
}
