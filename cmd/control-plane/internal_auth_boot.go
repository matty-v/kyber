package main

import "github.com/matty-v/kyber/pkg/telemetry"

// kyber#578 — cutover-safety for the internal API (:8082) per-agent auth posture.
//
// Postmortem (v2.1.0, 2026-06-14): the #566 control-plane deploy shipped
// enforce/fail-closed, but the out-of-band kyber-internal-signing-key Secret was
// never delivered. With no key the internal API fail-closed every /internal/...
// route (503) → all agent heartbeats + OAuth rotation refused → fleet outage, and
// the loud startup log went to NO alert channel, so it degraded silently for ~2h.
//
// The fail-closed-on-missing-key *posture* is intentional and stays. This adds
// defense in depth around the ROLLOUT: the chart defaults a new rollout to grace
// (L1, chart), a deploy gate aborts a keyless enforce cutover pre-prod (L2,
// preflight), and a keyless startup raises a one-shot alert (L3, here).
//
// CONSERVATIVE posture (Matt's gate decision, Q1 = NO): a missing key NEVER
// serves the internal API unauthenticated — it fails closed regardless of grace.
// The grace+no-key "accept-and-log unauthenticated transitional" relaxation is
// deliberately NOT implemented; it is only safe once kyber#586's alert-delivery
// path is live (otherwise an unauthenticated window would open without even a
// delivered page), so it is a follow-up. Fail-closed remains the floor.

// internalSigningKeySecretName is the out-of-band Secret carrying the cluster
// signing key (kyber#566). Named in the startup alert so an operator knows
// exactly what to deliver; the key VALUE is never logged or alerted.
const internalSigningKeySecretName = "kyber-internal-signing-key"

// internalAuthBootDecision is the pure outcome of the startup auth-posture
// decision, so the cutover-safety invariants are unit-testable without booting
// the control plane (cmd/control-plane is otherwise an integration surface).
type internalAuthBootDecision struct {
	// KeyPresent: the signing key was delivered → wire WithInternalAuth(authn,
	// Grace). When false, the caller wires WithInternalAuthFailClosed().
	KeyPresent bool
	// FailClosed: the internal API must refuse to serve (503) — true exactly when
	// the key is absent (conservative Q1=NO: never serve unauthenticated).
	FailClosed bool
	// Grace is the configured grace mode, threaded through for the authenticated
	// wiring (only meaningful when KeyPresent).
	Grace bool
	// Alert, when non-nil, is the single startup alert to fire (kyber#578 L3) —
	// nil only on the healthy key-present path so a valid deploy is not paged.
	Alert *telemetry.Alert
}

// decideInternalAuthBoot maps (keyPresent, grace) to the startup posture +
// optional one-shot alert. Pure; see internal_auth_boot_test.go for the matrix.
func decideInternalAuthBoot(keyPresent, grace bool) internalAuthBootDecision {
	d := internalAuthBootDecision{KeyPresent: keyPresent, Grace: grace}
	if keyPresent {
		// Healthy: enforce or grace, both authenticated. No alert — a correctly
		// delivered rollout proceeds normally (AC: valid deploy not blocked/paged).
		return d
	}

	// Key absent → fail closed (conservative, Q1=NO) AND alert once so the
	// fail-closed internal API pages instead of degrading silently. The impact
	// string names the Secret and the 503'd routes; it never carries key material.
	d.FailClosed = true
	impact := "internal API (:8082) FAILING CLOSED — every /internal/... route 503s " +
		"(agent heartbeats, OAuth rotation, session-brief fetch refused) until the " +
		internalSigningKeySecretName + " Secret is delivered"

	if grace {
		// A grace rollout missing its key: not a hard misconfig, but it still
		// fail-closes under the conservative posture, so the operator must deliver
		// the key before (or instead of) flipping to enforce. Warning severity +
		// a distinct reason so it reads differently from an enforce misconfig.
		d.Alert = &telemetry.Alert{
			Severity: "warning",
			Kind:     "control-plane",
			Name:     "internal-auth",
			Reason:   "InternalAuthGraceNoKey",
			Details: map[string]string{
				"secret": internalSigningKeySecretName,
				"impact": impact,
				"action": "deliver the " + internalSigningKeySecretName +
					" Secret and restart the control plane before flipping graceMode to enforce",
			},
		}
		return d
	}

	// Enforce with no key — the v2.1.0 incident shape. Critical.
	d.Alert = &telemetry.Alert{
		Severity: "critical",
		Kind:     "control-plane",
		Name:     "internal-auth",
		Reason:   "InternalAuthFailClosed",
		Details: map[string]string{
			"secret": internalSigningKeySecretName,
			"impact": impact,
			"action": "deliver the " + internalSigningKeySecretName +
				" Secret and restart the control plane",
		},
	}
	return d
}
