package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/matty-v/kyber/pkg/securityaudit"
)

type operatorCredentialUsageView struct {
	Version     int                                     `json:"version"`
	ObservedAt  string                                  `json:"observedAt"`
	Partial     bool                                    `json:"partial"`
	Truncated   bool                                    `json:"truncated"`
	EntryPoints []string                                `json:"entryPoints"`
	Identities  []securityaudit.CredentialUsageIdentity `json:"identities"`
	securityaudit.CredentialUsageView
	Local *securityaudit.CredentialUsageLocalStatus `json:"local,omitempty"`
}

func (s *Server) credentialUsageAvailable() bool {
	_, ok := s.SecurityAudit.(securityaudit.CredentialUsageReader)
	return ok && s.credentialUsageObserver() != nil
}
func (s *Server) handleOperatorCredentialUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "read-key-activity") {
		return
	}
	reader, ok := s.SecurityAudit.(securityaudit.CredentialUsageReader)
	observer := s.credentialUsageObserver()
	if !ok || observer == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "credential_usage_unavailable", "Key activity is unavailable.")
		return
	}
	resolved, err := s.auth.resolve(r.Context())
	if err != nil {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	actor := callerFrom(r.Context())
	views := []operatorCredentialView{}
	if resolved.registry != nil {
		entry, found := resolved.registry.byID[actor.CredentialID]
		if !found || !entry.caller.Scopes.full || !sameCredentialIdentity(entry.caller, *actor) {
			writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
			return
		}
		views = resolved.registry.inventory
	} else {
		current, authErr := resolved.authenticateConfigured(r)
		if authErr != nil || current == nil || !current.Scopes.full || !sameCredentialIdentity(*current, *actor) {
			writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
			return
		}
		views = resolved.credentialInventory()
	}
	identities := []securityaudit.CredentialUsageIdentity{}
	requested := []securityaudit.CredentialUsageIdentity{}
	seen := map[string]bool{}
	truncated := false
	for _, view := range views {
		if !view.ActivityTrackable {
			continue
		}
		identity := securityaudit.CredentialUsageIdentity{CredentialID: view.ID, PrincipalID: view.PrincipalID, TenantID: view.TenantID, Authority: "configuration"}
		key := identity.CredentialID + "\x00" + identity.PrincipalID + "\x00" + identity.TenantID
		if identity.Validate() != nil || seen[key] {
			continue
		}
		seen[key] = true
		if len(identities) >= securityaudit.MaxCredentialUsageItems/2 {
			truncated = true
			continue
		}
		if resolved.registry != nil {
			identity.Authority = "registry"
		}
		identities = append(identities, identity)
		requested = append(requested, identity)
		// Stable imported identity retains its observed pre-adoption history.
		if identity.Authority == "registry" {
			prior := identity
			prior.Authority = "configuration"
			requested = append(requested, prior)
		}
	}
	readCtx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	result, err := reader.ReadCredentialUsage(readCtx, s.Namespace, observer.CredentialUsageObserverID(), requested)
	cancel()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "credential_usage_unavailable", "Key activity could not be loaded. Try again.")
		return
	}
	// Do not emit protected metadata after a role/key change during the read.
	current, err := s.auth.Authenticate(r)
	if errors.Is(err, errOperatorAuthorityUnavailable) {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	if err != nil || current == nil || !current.Scopes.full || !sameCredentialIdentity(*current, *actor) {
		writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		return
	}
	response := operatorCredentialUsageView{Version: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Partial: true, Truncated: truncated, EntryPoints: []string{"operator-api", "browser-sign-in", "archive-download"}, Identities: identities, CredentialUsageView: result}
	if local, ok := observer.(interface {
		CredentialUsageLocalStatus() securityaudit.CredentialUsageLocalStatus
	}); ok {
		status := local.CredentialUsageLocalStatus()
		response.Local = &status
	}
	writeJSON(w, http.StatusOK, response)
}
