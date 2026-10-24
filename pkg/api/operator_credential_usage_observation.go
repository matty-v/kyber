package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/securityaudit"
)

func (s *Server) credentialUsageObserver() securityaudit.CredentialUsageObserver {
	observer := s.OperatorCredentialUsage
	if observer == nil || observer.CredentialUsageNamespace() != s.Namespace {
		return nil
	}
	return observer
}
func observeUnidentifiedCredentialUsage(observer securityaudit.CredentialUsageObserver, outcome string) {
	if observer != nil {
		observer.ObserveCredentialUsage(securityaudit.CredentialUsageObservation{Outcome: outcome})
	}
}
func credentialUsageIdentity(caller *Caller) securityaudit.CredentialUsageIdentity {
	if caller == nil {
		return securityaudit.CredentialUsageIdentity{}
	}
	authority := caller.CredentialAuthority
	if authority == "" {
		authority = "configuration"
	}
	return securityaudit.CredentialUsageIdentity{CredentialID: caller.CredentialID, PrincipalID: caller.PrincipalID, TenantID: caller.TenantID, Authority: authority}
}
func credentialUsageTransport(r *http.Request) string {
	if r.Header.Get("Authorization") == "" {
		if cookie, err := r.Cookie(browserSessionCookie); err == nil && cookie.Value != "" {
			return "browser-session"
		}
	}
	return "bearer" // Includes API-key query authentication on WebSocket upgrades.
}
func observeAcceptedCredentialUsage(observer securityaudit.CredentialUsageObserver, caller *Caller, transport, action string, unique bool) {
	if observer == nil {
		return
	}
	identity := credentialUsageIdentity(caller)
	if !unique || caller == nil || identity.CredentialID == "" || identity.PrincipalID == "" || identity.TenantID == "" || identity.Validate() != nil || caller.CredentialGeneration == 0 {
		observeUnidentifiedCredentialUsage(observer, "unattributed")
		return
	}
	observer.ObserveCredentialUsage(securityaudit.CredentialUsageObservation{CredentialUsageIdentity: identity, Transport: transport, Outcome: "accepted", Action: action, At: time.Now().UTC(), Generation: caller.CredentialGeneration})
}

// The wrapper keeps the existing streaming/hijacking/deadline interfaces and
// content-type protection. A valid authentication and a denied HTTP operation
// are distinct observations; no successful action completion is inferred.
type credentialUsageResponseWriter struct {
	*responseWriter
	observer  securityaudit.CredentialUsageObserver
	identity  securityaudit.CredentialUsageIdentity
	transport string
}

func (w *credentialUsageResponseWriter) WriteHeader(status int) {
	if !w.wrote && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if w.identity.Validate() != nil {
			observeUnidentifiedCredentialUsage(w.observer, "unrecognized")
		} else {
			w.observer.ObserveCredentialUsage(securityaudit.CredentialUsageObservation{CredentialUsageIdentity: w.identity, Transport: w.transport, Outcome: "denied"})
		}
	}
	w.responseWriter.WriteHeader(status)
}
func observeCredentialUsageResponse(w http.ResponseWriter, observer securityaudit.CredentialUsageObserver, caller *Caller, transport string, unique bool) http.ResponseWriter {
	if observer == nil {
		return w
	}
	identity := credentialUsageIdentity(caller)
	if !unique || caller == nil || caller.CredentialGeneration == 0 || identity.Validate() != nil {
		identity = securityaudit.CredentialUsageIdentity{}
	}
	return &credentialUsageResponseWriter{responseWriter: newResponseWriter(w), observer: observer, identity: identity, transport: transport}
}

// Fixed request categories contain no resource names, raw paths or query data.
// They describe authenticated requests, including requests later denied/failed.
func credentialUsageRequestAction(r *http.Request) string {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
	path := r.URL.Path
	for _, kind := range []string{"agents", "machines"} {
		prefix := "/api/v1/" + kind + "/"
		if strings.HasPrefix(path, prefix) {
			tail := strings.TrimPrefix(path, prefix)
			if slash := strings.IndexByte(tail, '/'); slash >= 0 {
				sub := tail[slash:]
				switch {
				case strings.HasPrefix(sub, "/archives"):
					if read {
						return "archives:read"
					}
					return "archives:write"
				case sub == "/exec":
					return kind + ":exec"
				case sub == "/secrets" && read:
					return "secrets:list"
				case strings.HasPrefix(sub, "/secrets/") || sub == "/secrets":
					if read {
						return "secrets:read"
					}
					return "secrets:write"
				case strings.HasPrefix(sub, "/transcripts"):
					return "transcripts:read"
				case strings.HasPrefix(sub, "/logs"):
					if r.URL.Query().Get("source") == "transcript" {
						return "transcripts:read"
					}
					return "logs:read"
				}
			}
		}
	}
	groups := []struct{ prefix, kind string }{
		{"/api/v1/operator-credentials", "credentials"}, {"/api/v1/operator-credential-operations", "credentials"}, {"/api/v1/rotate-api-key", "credentials"},
		{"/api/v1/agents", "agents"}, {"/api/v1/machines", "machines"}, {"/api/v1/tasks", "tasks"}, {"/api/v1/requests", "requests"},
		{"/api/v1/archives", "archives"},
	}
	for _, group := range groups {
		if path == group.prefix || strings.HasPrefix(path, group.prefix+"/") {
			if read {
				return group.kind + ":read"
			}
			return group.kind + ":write"
		}
	}
	if read {
		if strings.HasPrefix(path, "/api/v1/usage") {
			return "usage:read"
		}
		if path == "/api/v1/security-events" {
			return "audit:read"
		}
		if strings.HasPrefix(path, "/api/v1/logs") {
			return "logs:read"
		}
		return "api:read"
	}
	return "api:write"
}

// This only controls attribution, never authentication/authorization. Distinct
// accepted values claiming the same identity cannot be distinguished without
// retaining material, so their observations remain unattributed instead.
func (a *APIKeyAuthenticator) credentialUsageIdentityUnique(caller *Caller) bool {
	if caller == nil || credentialUsageIdentity(caller).Validate() != nil || caller.CredentialGeneration == 0 {
		return false
	}
	if caller.CredentialAuthority == "registry" {
		return true
	} // Registry validation enforces unique IDs.
	a.mu.RLock()
	defer a.mu.RUnlock()
	winners := map[string]securityaudit.CredentialUsageIdentity{}
	for _, entry := range a.callers {
		winners[entry.key] = credentialUsageIdentity(&entry.caller)
	}
	if a.key != "" {
		winners[a.key] = credentialUsageIdentity(installationAdminCaller())
	}
	expected := credentialUsageIdentity(caller)
	matches := 0
	for _, identity := range winners {
		if identity == expected {
			matches++
		}
	}
	return matches == 1
}
