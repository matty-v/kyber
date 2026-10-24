package api

import "net/http"

// requireManagementAudit is an additional required-before-effect guard. Existing
// handlers retain their validation and non-leaky authorization boundaries.
func (s *Server) requireManagementAudit(w http.ResponseWriter, r *http.Request, action, kind, name string, scope Scope, installationWide bool) bool {
	check := func(c *Caller) bool {
		if c == nil {
			return false
		}
		if scope == "" {
			return c.Scopes.full
		}
		if !c.Scopes.Has(scope) {
			return false
		}
		resources := c.AgentResources
		if kind == "machine" {
			resources = c.MachineResources
		}
		if installationWide {
			return resources.all
		}
		return resources.Has(s.Namespace + "/" + name)
	}
	return s.requireManagementAuditWithCheck(w, r, action, kind, name, check)
}

func (s *Server) requireManagementAuditWithCheck(w http.ResponseWriter, r *http.Request, action, kind, name string, check func(*Caller) bool) bool {
	w.Header().Set("Cache-Control", "no-store")
	caller := callerFrom(r.Context())
	allowed := caller != nil && check(caller)
	if allowed && (caller.PrincipalID == "" || caller.CredentialID == "" || caller.CredentialGeneration == 0) {
		s.auditRequiredOperatorDecision(w, r, action, kind, name, false, check)
		writeJSONError(w, http.StatusForbidden, "credential_identity_required", "This key is missing identification details required for security history. An administrator must update its configuration.")
		return false
	}
	if !s.auditRequiredOperatorDecision(w, r, action, kind, name, allowed, check) {
		return false
	}
	if !allowed {
		writeJSONError(w, http.StatusForbidden, "forbidden", "access to this operation is not granted")
	}
	return allowed
}

func (s *Server) requireAgentCreateAudit(w http.ResponseWriter, r *http.Request, name string) bool {
	if archiveHoldFrom(r.Context()) != nil {
		// Only the private import context may reuse an already admitted import.
		// A nested create must not replace its parent operation or require a second
		// unrelated permission after the import job has already been reserved.
		state, _ := r.Context().Value(sensitiveAuditContextKey{}).(*sensitiveAuditState)
		actor := callerFrom(r.Context())
		if actor != nil && state != nil && state.admission != nil && state.admission.Action == "archives:import" && state.admission.ResourceKind == "agent" && state.admission.ResourceName == name && state.admission.Namespace == s.Namespace && state.admission.PrincipalID == actor.PrincipalID && state.admission.CredentialID == actor.CredentialID && state.admission.Generation == actor.CredentialGeneration {
			return true
		}
		writeJSONError(w, http.StatusServiceUnavailable, "security_audit_unavailable", "security history is unavailable; this operation was not started")
		return false
	}
	return s.requireManagementAudit(w, r, "lifecycle:write", "agent", name, "", false)
}

// An attempted remote write is uncertain until acknowledged. Do not overwrite
// a parent import's outcome when its nested Agent creation succeeds.
func beginManagementMutation(r *http.Request) { markSensitiveOutcome(r, "unknown") }
func completeManagementMutation(r *http.Request, action string) {
	state, _ := r.Context().Value(sensitiveAuditContextKey{}).(*sensitiveAuditState)
	if state != nil && state.admission != nil && state.admission.Action == action {
		markSensitiveOutcome(r, "success")
	}
}
