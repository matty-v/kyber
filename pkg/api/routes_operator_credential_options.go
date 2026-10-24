package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/matty-v/kyber/pkg/securityaudit"
)

type operatorCredentialPermissionOption struct {
	Scope  string `json:"scope"`
	Label  string `json:"label"`
	Target string `json:"target"`
}
type operatorCredentialOptions struct {
	Version     int                                  `json:"version"`
	Namespace   string                               `json:"namespace"`
	Permissions []operatorCredentialPermissionOption `json:"permissions"`
}

// This is shared with mutation validation. UI choices must follow the same
// exact-target requirements rather than maintaining a second permission policy.
func managedPermissionTarget(scope Scope) string {
	switch scope {
	case ScopeLifecycleWrite, ScopeLifecycleAdmin, ScopeAgentExec, ScopeSecretsList, ScopeSecretsRead, ScopeSecretsWrite, ScopeLogsRead, ScopeTranscriptsRead, ScopeArchivesAdmin, ScopeRequestsWrite, ScopeTasksCreate, ScopeCapabilitiesWrite:
		return "agent"
	case ScopeMachineExec, ScopeMachineLogsRead:
		return "machine"
	default:
		return "none"
	}
}

var managedPermissionLabels = []struct {
	scope Scope
	label string
}{
	{ScopeMetricsRead, "Read fleet metrics"}, {ScopeUsageRead, "Read token usage"},
	{ScopeCapabilitiesRead, "Read published Agent capabilities"},
	{ScopeLifecycleWrite, "Start, stop and configure Agents"}, {ScopeLifecycleAdmin, "Manage and delete Agents"},
	{ScopeAgentExec, "Open Agent terminals"}, {ScopeLogsRead, "Read Agent logs"}, {ScopeTranscriptsRead, "Read Agent conversations"},
	{ScopeSecretsList, "List Agent credential names"}, {ScopeSecretsRead, "Read Agent credential values"}, {ScopeSecretsWrite, "Change Agent credentials"},
	{ScopeArchivesAdmin, "Export and import Agent disks"}, {ScopeCapabilitiesWrite, "Publish Agent capabilities"},
	{ScopeTasksCreate, "Submit tasks"}, {ScopeTasksRead, "Read owned tasks"}, {ScopeTasksList, "List owned tasks"},
	{ScopeTaskResultsRead, "Read owned task results"}, {ScopeTaskEventsRead, "Read owned task events"},
	{ScopeTasksContinue, "Respond to owned task interactions"}, {ScopeTasksCancel, "Cancel owned tasks"},
	{ScopeRequestsWrite, "Submit Agent requests"}, {ScopeRequestsRead, "Read owned Agent requests"},
	{ScopeMachineLogsRead, "Read Machine logs"}, {ScopeMachineExec, "Open Machine terminals"},
}

func (s *Server) handleOperatorCredentialOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "read-key-options") {
		return
	}
	authority, _, available := s.credentialManagementDependencies()
	if !available {
		writeCredentialManagementUnavailable(w)
		return
	}
	snapshot, err := authority.Load(r.Context())
	if err != nil || snapshot.Document.Namespace != s.Namespace {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	if err := currentManagedAdministrator(snapshot, callerFrom(r.Context())); err != nil {
		if errors.Is(err, errCredentialMutationForbidden) {
			writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		} else {
			writeOperatorAuthenticationUnavailable(w)
		}
		return
	}
	result := operatorCredentialOptions{Version: 1, Namespace: s.Namespace, Permissions: []operatorCredentialPermissionOption{}}
	for _, permission := range managedPermissionLabels {
		result.Permissions = append(result.Permissions, operatorCredentialPermissionOption{Scope: string(permission.scope), Label: permission.label, Target: managedPermissionTarget(permission.scope)})
	}
	writeJSON(w, http.StatusOK, result)
}

type operatorCredentialPendingPage struct {
	Items []credentialOperationView `json:"items"`
	Next  string                    `json:"next,omitempty"`
}

func (s *Server) handlePendingOperatorCredentialOperations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "read-pending-key-changes") {
		return
	}
	authority, journal, available := s.credentialManagementDependencies()
	if !available {
		writeCredentialManagementUnavailable(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	operations, err := journal.PendingCredentialOperations(ctx, s.Namespace, r.URL.Query().Get("after"), 26)
	if errors.Is(err, securityaudit.ErrInvalid) {
		writeJSONError(w, http.StatusBadRequest, "invalid_cursor", "Refresh the pending changes list.")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "credential_operation_unavailable", "Pending changes could not be checked.")
		return
	}
	snapshot, err := authority.Load(r.Context())
	if err != nil || snapshot.Document.Namespace != s.Namespace {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	if err := currentManagedAdministrator(snapshot, callerFrom(r.Context())); err != nil {
		if errors.Is(err, errCredentialMutationForbidden) {
			writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		} else {
			writeOperatorAuthenticationUnavailable(w)
		}
		return
	}
	result := operatorCredentialPendingPage{Items: []credentialOperationView{}}
	if len(operations) > 25 {
		operations = operations[:25]
		result.Next = operations[24].Intent.Admission.OperationID
	}
	for _, operation := range operations {
		result.Items = append(result.Items, credentialOperationProjection(operation))
	}
	writeJSON(w, http.StatusOK, result)
}

// Only an acknowledged self-rotation in cookie mode refreshes that same
// credential's browser session. Other keys never change the caller's identity.
func (s *Server) refreshManagedRotationSession(w http.ResponseWriter, r *http.Request, request credentialMutationRequest, actor Caller) {
	if request.Action != "rotate" || request.CredentialID != actor.CredentialID || r.Header.Get("Authorization") != "" {
		return
	}
	if _, err := r.Cookie(browserSessionCookie); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), securityAuditTimeout)
	defer cancel()
	resolved, err := s.auth.resolve(ctx)
	if err != nil || resolved.registry == nil {
		return
	}
	caller, err := resolved.registry.authenticate(registryRotationBearer(request.Key))
	if err != nil || caller.CredentialID != actor.CredentialID || caller.PrincipalID != actor.PrincipalID || caller.TenantID != actor.TenantID || caller.CredentialGeneration != actor.CredentialGeneration+1 {
		return
	}
	token, err := resolved.registry.createBrowserSession(*caller, time.Now())
	if err == nil {
		setBrowserSessionCookie(w, r, token)
	}
}
func registryRotationBearer(key string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://localhost/", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}
