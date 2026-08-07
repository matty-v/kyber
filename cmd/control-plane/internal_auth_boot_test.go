package main

import "testing"

// kyber#578 — the cutover-safety decision for the internal API (:8082) auth
// posture at control-plane startup. These tests pin the CONSERVATIVE posture
// Matt locked at the gate (Q1 = NO): a missing signing key NEVER serves the
// internal API unauthenticated — it fails closed regardless of grace mode — and
// a keyless startup ALWAYS raises exactly one startup alert (never silent, the
// v2.1.0 fail-closed-for-~2h incident). The grace+no-key "accept-and-log"
// relaxation is deliberately NOT implemented here; it is a follow-up gated on
// kyber#586's alert-delivery path being live.

func TestDecideInternalAuthBoot_KeyPresent_NoAlert(t *testing.T) {
	for _, grace := range []bool{false, true} {
		d := decideInternalAuthBoot(true, grace)
		if !d.KeyPresent {
			t.Errorf("grace=%v: KeyPresent must be true when the key is delivered", grace)
		}
		if d.FailClosed {
			t.Errorf("grace=%v: must NOT fail closed when the key is present", grace)
		}
		if d.Alert != nil {
			t.Errorf("grace=%v: a correctly-delivered rollout must not raise an alert; got %+v", grace, d.Alert)
		}
		if d.Grace != grace {
			t.Errorf("grace=%v: Grace must be threaded through for the authenticated wiring", grace)
		}
	}
}

func TestDecideInternalAuthBoot_KeyAbsentEnforce_FailClosedCriticalAlert(t *testing.T) {
	d := decideInternalAuthBoot(false, false)
	if !d.FailClosed {
		t.Fatal("key absent + enforce must fail closed (posture unchanged)")
	}
	if d.KeyPresent {
		t.Error("KeyPresent must be false when the key is absent")
	}
	if d.Alert == nil {
		t.Fatal("key absent + enforce must raise a startup alert (AC2 — never silent)")
	}
	if d.Alert.Severity != "critical" {
		t.Errorf("enforce keyless fail-closed must be critical; got %q", d.Alert.Severity)
	}
	if d.Alert.Reason != "InternalAuthFailClosed" {
		t.Errorf("reason = %q, want InternalAuthFailClosed", d.Alert.Reason)
	}
	assertAlertNamesSecretAndImpact(t, d)
}

func TestDecideInternalAuthBoot_KeyAbsentGrace_FailClosedWarningAlert(t *testing.T) {
	// CONSERVATIVE (Q1 = NO): grace + no key STILL fails closed — it does NOT
	// accept-and-log unauthenticated. The only difference from enforce is the
	// alert (warning + a distinct reason telling the operator to deliver the key
	// before flipping to enforce).
	d := decideInternalAuthBoot(false, true)
	if !d.FailClosed {
		t.Fatal("CONSERVATIVE Q1=NO: grace + no key must STILL fail closed (no unauthenticated window)")
	}
	if d.Alert == nil {
		t.Fatal("grace + no key must raise a startup alert (never silent)")
	}
	if d.Alert.Severity != "warning" {
		t.Errorf("grace keyless alert should be warning (distinct from enforce critical); got %q", d.Alert.Severity)
	}
	if d.Alert.Reason != "InternalAuthGraceNoKey" {
		t.Errorf("reason = %q, want InternalAuthGraceNoKey", d.Alert.Reason)
	}
	assertAlertNamesSecretAndImpact(t, d)
}

// assertAlertNamesSecretAndImpact is AC2: the alert names the missing Secret and
// the impact (internal routes 503'ing), and AC: never leaks the key value (there
// is no key, but assert no field is the literal key material placeholder).
func assertAlertNamesSecretAndImpact(t *testing.T, d internalAuthBootDecision) {
	t.Helper()
	if got := d.Alert.Details["secret"]; got != internalSigningKeySecretName {
		t.Errorf("alert must name the %q Secret; details.secret=%q", internalSigningKeySecretName, got)
	}
	impact := d.Alert.Details["impact"]
	if impact == "" {
		t.Error("alert must describe the impact (internal /internal/... routes 503'ing)")
	}
	for _, want := range []string{"503", "/internal/"} {
		if !contains(impact, want) {
			t.Errorf("impact %q must mention %q (the 503'd internal routes)", impact, want)
		}
	}
	if d.Alert.Kind == "" || d.Alert.Name == "" {
		t.Error("alert must set Kind and Name so the sink can route/label it")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
