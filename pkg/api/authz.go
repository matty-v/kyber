package api

import (
	"log/slog"
	"net/http"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Caller-level authorization for agent lifecycle mutations (kyber#474).
//
// Authentication (auth.go) answers "is this a valid key?"; authorization here
// answers "may THIS caller drive THIS verb?". The check is enforced server-side
// at the setAgentDesiredPhase chokepoint (and the OAuth re-auth resume), so it
// holds regardless of how the verb is reached — it cannot be bypassed by hitting
// a route directly. The controller's classifyEvent effect-allowlist
// (reconciler.go) stays as complementary defense-in-depth on the *effect*; this
// is the *caller* gate.

// requiredScopeForPhase maps a desired-phase mutation to the scope it requires.
// Fail-safe verbs (start/stop/restart, and the OAuth re-auth resume to Running)
// require only lifecycle:write. The impactful, less-fail-safe verbs — suspend
// and force-needs-auth (NeedsAuth) — require the strictly-higher lifecycle:admin
// (admin ⊃ write). This ordering is the #474 security invariant: the impactful
// verbs can never be less-protected than fail-safe Stop.
func requiredScopeForPhase(phase kyberv1.AgentPhase) Scope {
	switch phase {
	case kyberv1.AgentPhaseSuspended, kyberv1.AgentPhaseNeedsAuth:
		return ScopeLifecycleAdmin
	default:
		// Running (start / oauth resume), Stopped (stop), Restarting (restart).
		return ScopeLifecycleWrite
	}
}

// authorizePhase enforces caller-level authorization for a desired-phase
// mutation. It audit-logs every decision (caller, agent, phase, outcome, mode)
// and returns true if the request may proceed. On a denied caller it returns
// false and — only when enforcement is on — writes a non-leaky 403; in permissive
// mode (the default) it logs a would-deny and allows the request through so
// existing single-key installs are unaffected and operators can observe before
// enforcing.
func (s *Server) authorizePhase(w http.ResponseWriter, r *http.Request, agentName string, phase kyberv1.AgentPhase) bool {
	return s.authorizeAction(w, r, agentName, "phase:"+string(phase), requiredScopeForPhase(phase))
}

// authorizeAction is the shared caller-gate that authorizePhase delegates to and
// that the destructive DELETE paths call directly (kyber#565). It audit-logs
// every decision (caller, agent, action, required scope, outcome, mode) and
// returns true if the request may proceed. On a denied caller it returns false
// and — only when enforcement is on — writes a non-leaky 403; in permissive mode
// (the default) it logs a would-deny and allows the request through so existing
// single-key installs are unaffected and operators can observe before enforcing.
//
// action is a short, audit-only label for the verb being gated (e.g.
// "phase:Suspended", "delete", "machine-delete"); it never affects the decision,
// only the log line.
func (s *Server) authorizeAction(w http.ResponseWriter, r *http.Request, agentName, action string, required Scope) bool {
	caller := callerFrom(r.Context())

	callerName := "unknown"
	allowed := false
	if caller != nil {
		callerName = caller.Name
		allowed = caller.Scopes.Has(required)
	}

	if allowed {
		slog.Info("authz allow",
			"caller", callerName, "agent", agentName,
			"action", action, "required_scope", string(required),
			"decision", "allow", "enforce", s.AuthzEnforce)
		return true
	}

	if !s.AuthzEnforce {
		// Permissive mode: observe-only. Log a would-deny and allow through so
		// the migration is safe (operators see who would be denied first).
		slog.Warn("authz would-deny (permissive mode)",
			"caller", callerName, "agent", agentName,
			"action", action, "required_scope", string(required),
			"decision", "would-deny", "enforce", false)
		return true
	}

	slog.Warn("authz deny",
		"caller", callerName, "agent", agentName,
		"action", action, "required_scope", string(required),
		"decision", "deny", "enforce", true)
	// Non-leaky: name neither the required scope nor the caller's granted scopes.
	writeJSONError(w, http.StatusForbidden, "forbidden", "insufficient scope for this action")
	return false
}
