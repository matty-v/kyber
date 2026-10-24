package api

import (
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// operatorBoundary denies restricted credentials on operator routes that have
// not explicitly opted into a restricted action/resource policy. It runs after
// authentication and before route dispatch or backend reads. Subrouters listed
// below retain their own finer-grained ownership and method checks.
func (s *Server) operatorBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := callerFrom(r.Context())
		if caller != nil && (caller.Scopes.full || s.restrictedOperatorRoute(r, caller)) {
			next.ServeHTTP(w, r)
			return
		}
		name, principal, credential := "unknown", "", ""
		generation := uint64(0)
		if caller != nil {
			name, principal, credential = caller.Name, caller.PrincipalID, caller.CredentialID
			generation = caller.CredentialGeneration
		}
		action, resource := "operator-route", ""
		auditAction, auditKind, auditName := "operator:unclassified", "installation", ""
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/security-events" {
			auditAction = "audit:read"
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/api/v1/logging/logs" || r.URL.Path == "/api/v1/logging/export") {
			auditAction = string(ScopeLogsRead)
		}

		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/rotate-api-key" {
			auditAction, auditKind, auditName = "credential:rotate", "credential", installationAdminCredentialID
		}
		if r.URL.Path == "/api/v1/agent-imports" && r.Method == http.MethodPost {
			auditAction = "archives:import"
		}
		if r.URL.Path == "/api/v1/archives" && r.Method == http.MethodGet {
			auditAction = "archives:read"
		}
		if r.URL.Path == "/api/v1/archive-uploads" && r.Method == http.MethodPost {
			auditAction = "archives:upload"
		}
		// Classify only known operations and validated names. Never log query
		// strings, command arguments or unknown path content.
		for _, kind := range []string{"agents", "machines"} {
			prefix := "/api/v1/" + kind + "/"
			if strings.HasPrefix(r.URL.Path, prefix) {
				target, operation, ok := splitAction(strings.TrimPrefix(r.URL.Path, prefix))
				if ok && isValidName(target) {
					scope, known := agentSensitiveScope(r, operation)
					if kind == "machines" {
						scope, known = machineSensitiveScope(r, operation)
					}

					if !known {
						mutation := ""
						if r.Method == http.MethodDelete && operation == "" {
							mutation = "lifecycle:delete"
						}
						if r.Method == http.MethodPost {
							switch operation {
							case "start", "stop", "restart", "force-needs-auth", "set-model", "set-runtime-version", "set-resources", "increase-disk", "restart-session", "compact-session", "repair-runtime", "switch-runtime":
								if kind == "agents" {
									mutation = "lifecycle:write"
								}
							case "retry-cost-optimized", "restart-agents":
								if kind == "machines" {
									mutation = "lifecycle:write"
								}
							}
							if kind == "machines" && (operation == "start" || operation == "stop" || operation == "restart") {
								mutation = "lifecycle:write"
							}
						}
						if mutation != "" {
							scope, known = Scope(mutation), true
						}
					}
					if known {
						action, resource = string(scope), s.Namespace+"/"+target
						auditAction, auditName = string(scope), target
						if kind == "machines" {
							auditKind = "machine"
						} else {
							auditKind = "agent"
						}
					}
				}
			}
		}
		slog.Warn("authz deny", "caller", name, "principal_id", principal, "credential_id", credential, "credential_generation", generation, "action", action, "resource", resource, "decision", "deny", "enforce", true)
		s.auditOperatorDenial(w, r, auditAction, auditKind, auditName)
		w.Header().Set("Cache-Control", "no-store")
		writeJSONError(w, http.StatusForbidden, "forbidden", "access to this operation is not granted")
	})
}

func (s *Server) restrictedOperatorRoute(r *http.Request, caller *Caller) bool {
	p := r.URL.Path
	// Do not allow ServeMux cleaning/redirects to transform an admitted path
	// into a different operation. Decoding has already happened in URL.Path.
	if path.Clean(p) != strings.TrimSuffix(p, "/") {
		return false
	}
	if p == "/api/v1/version" {
		return r.Method == http.MethodGet
	}
	if p == "/api/v1/usage/events" || p == "/api/v1/usage/aggregates" {
		return true // usage handlers enforce scope and current Agent access
	}
	if strings.HasPrefix(p, "/api/v1/metrics/") {
		if r.Method != http.MethodGet || !caller.Scopes.Has(ScopeMetricsRead) {
			return false
		}
		switch strings.TrimPrefix(p, "/api/v1/metrics/") {
		case "summary", "activity", "working-time", "last-active", "nodes", "state-changes":
			return true
		default:
			return false
		}
	}
	for _, prefix := range []string{"/api/v1/archives", "/api/v1/archive-uploads", "/api/v1/agent-imports"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return caller.Scopes.Has(ScopeArchivesAdmin)
		}
	}
	if strings.HasPrefix(p, "/a2a/v1/agents/") {
		return true // protocol handlers enforce task/capability resource policy
	}
	if strings.HasPrefix(p, "/api/v1/machines/") {
		name, action, ok := splitAction(strings.TrimPrefix(p, "/api/v1/machines/"))
		if !ok || !isValidName(name) || !caller.MachineResources.Has(s.Namespace+"/"+name) {
			return false
		}
		scope, known := machineSensitiveScope(r, action)
		return known && caller.Scopes.Has(scope)
	}
	const prefix = "/api/v1/agents/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	name, action, ok := splitAction(strings.TrimPrefix(p, prefix))
	if !ok || !isValidName(name) {
		return false
	}
	for _, subtree := range []string{"tasks", "exports"} {
		if action == subtree || strings.HasPrefix(action, subtree+"/") {
			return true // existing strict task/archive authorization owns these
		}
	}
	if action == "capabilities" {
		return true // existing exact-resource capability policy
	}
	if !caller.AgentResources.Has(s.Namespace + "/" + name) {
		return false
	}
	if action == "" {
		switch r.Method {
		case http.MethodDelete:
			return caller.Scopes.Has(ScopeLifecycleAdmin)
		case http.MethodPatch:
			// patchAgent admits only an isolated publicCapabilities field for
			// restricted writers and returns no private Agent representation.
			return caller.Scopes.Has(ScopeCapabilitiesWrite)
		default:
			return false
		}
	}
	if scope, known := agentSensitiveScope(r, action); known {
		return caller.Scopes.Has(scope)
	}
	if action == "requests" || strings.HasPrefix(action, "requests/") {
		return true // request action scopes are checked by the handler
	}
	if r.Method != http.MethodPost {
		return false
	}
	switch action {
	case "start", "stop", "restart", "set-model", "set-runtime-version", "set-resources", "increase-disk", "restart-session", "compact-session", "repair-runtime":
		return caller.Scopes.Has(ScopeLifecycleWrite)
	case "force-needs-auth":
		return caller.Scopes.Has(ScopeLifecycleAdmin)
	default:
		return false
	}
}
