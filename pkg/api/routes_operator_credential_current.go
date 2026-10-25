package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Only the authenticated credential's current metadata, never key material or
// an installation inventory. Empty identities are not repaired by this read.
type operatorCredentialCurrentView struct {
	ObservedAt         string   `json:"observedAt"`
	ControlPlanePodUID string   `json:"controlPlanePodUid,omitempty"`
	Namespace          string   `json:"namespace"`
	Name               string   `json:"name"`
	CredentialID       string   `json:"credentialId,omitempty"`
	PrincipalID        string   `json:"principalId,omitempty"`
	TenantID           string   `json:"tenantId,omitempty"`
	Kind               string   `json:"kind,omitempty"`
	Issuer             string   `json:"issuer,omitempty"`
	Subject            string   `json:"subject,omitempty"`
	Generation         string   `json:"generation"`
	IdentityComplete   bool     `json:"identityComplete"`
	Authority          string   `json:"authority"`
	AccessLevel        string   `json:"accessLevel"`
	Scopes             []string `json:"scopes"`
	AgentResources     []string `json:"agentResources"`
	MachineResources   []string `json:"machineResources"`
}

func (s *Server) handleCurrentOperatorCredential(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "GET required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	// Do not reuse the earlier authentication/route snapshot. This read is about
	// what the presented credential is accepted as now, including narrowed access.
	current, err := s.auth.Authenticate(r.WithContext(ctx))
	if errors.Is(err, errOperatorAuthorityUnavailable) {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	if err != nil || current == nil {
		writeJSONError(w, 401, "unauthorized", "This credential is no longer accepted.")
		return
	}
	if ctx.Err() != nil {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	writeJSON(w, 200, currentOperatorCredentialView(*current, s.Namespace, s.OperatorCredentialPodUID, time.Now().UTC()))
}

func currentOperatorCredentialView(current Caller, namespace, podUID string, now time.Time) operatorCredentialCurrentView {
	authority := current.CredentialAuthority
	if authority == "" {
		authority = "configuration"
	}
	scopes := []string{}
	for scope, enabled := range current.Scopes.scopes {
		if enabled {
			scopes = append(scopes, string(scope))
		}
	}
	sort.Strings(scopes)
	role := "restricted"
	if current.Scopes.full {
		role = "administrator"
		scopes = []string{}
	}
	response := operatorCredentialCurrentView{ObservedAt: now.Format(time.RFC3339Nano), Namespace: namespace, Name: current.Name, CredentialID: current.CredentialID, PrincipalID: current.PrincipalID, TenantID: current.TenantID, Kind: current.Kind, Issuer: current.Issuer, Subject: current.Subject, Generation: strconv.FormatUint(current.CredentialGeneration, 10), IdentityComplete: current.CredentialID != "" && current.PrincipalID != "" && current.TenantID != "" && current.CredentialGeneration > 0, Authority: authority, AccessLevel: role, Scopes: scopes, AgentResources: sortedResources(current.AgentResources), MachineResources: sortedResources(current.MachineResources)}
	if id, err := uuid.Parse(podUID); err == nil && id != uuid.Nil && id.String() == podUID {
		response.ControlPlanePodUID = id.String()
	}
	return response
}
