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
		name := "unknown"
		if caller != nil {
			name = caller.Name
		}
		slog.Warn("authz deny", "caller", name, "action", "operator-route", "decision", "deny", "enforce", true)
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
