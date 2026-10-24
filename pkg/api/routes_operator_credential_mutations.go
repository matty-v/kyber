package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type managedOperatorCredentialAuthority interface {
	OperatorCredentialAuthority
	Replace(context.Context, string, string, operatorcredentials.Document) (operatorcredentials.Snapshot, error)
	Fence(context.Context, string, string) (operatorcredentials.Snapshot, error)
}

type credentialOperationView struct {
	OperationID  string `json:"operationId"`
	Action       string `json:"action"`
	CredentialID string `json:"credentialId"`
	Status       string `json:"status"`
	ObservedAt   string `json:"observedAt"`
}

func credentialOperationProjection(operation securityaudit.CredentialOperation) credentialOperationView {
	admission := operation.Intent.Admission
	view := credentialOperationView{OperationID: admission.OperationID, Action: admission.Action, CredentialID: admission.ResourceName, Status: "pending", ObservedAt: admission.ObservedAt.UTC().Format(time.RFC3339Nano)}
	if operation.Completion != nil {
		view.Status = operation.Completion.Outcome
		view.ObservedAt = operation.Completion.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	return view
}
func credentialRegistryETag(uid, version string) string {
	// Hash metadata only. A reviewed version from another registry must not
	// match merely because two clusters share the same opaque resource version.
	encoded, _ := json.Marshal([2]string{uid, version})
	digest := sha256.Sum256(encoded)
	return `"` + base64.RawURLEncoding.EncodeToString(digest[:]) + `"`
}
func (s *Server) credentialManagementDependencies() (managedOperatorCredentialAuthority, securityaudit.CredentialJournal, bool) {
	authority, ok := s.OperatorCredentialAuthority.(managedOperatorCredentialAuthority)
	journal, hasJournal := s.SecurityAudit.(securityaudit.CredentialJournal)
	return authority, journal, ok && hasJournal
}
func writeCredentialManagementUnavailable(w http.ResponseWriter) {
	writeJSONError(w, http.StatusServiceUnavailable, "credential_management_unavailable", "Key management is unavailable. No new key change was started.")
}

func decodeCredentialMutationRequest(w http.ResponseWriter, r *http.Request) (credentialMutationRequest, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil {
		return credentialMutationRequest{}, errCredentialMutationInvalid
	}
	unique := json.NewDecoder(bytes.NewReader(raw))
	unique.UseNumber()
	var value any
	if decodeUniqueJSONValue(unique, &value, 0) != nil {
		return credentialMutationRequest{}, errCredentialMutationInvalid
	}
	if _, err := unique.Token(); err != io.EOF {
		return credentialMutationRequest{}, errCredentialMutationInvalid
	}
	object, ok := value.(map[string]any)
	if !ok {
		return credentialMutationRequest{}, errCredentialMutationInvalid
	}
	allowed := map[string]bool{"operationId": true, "action": true, "credentialId": true, "policy": true, "key": true}
	for field := range object {
		if !allowed[field] {
			return credentialMutationRequest{}, errCredentialMutationInvalid
		}
	}
	if rawPolicy, ok := object["policy"]; ok && rawPolicy != nil {
		policy, ok := rawPolicy.(map[string]any)
		if !ok {
			return credentialMutationRequest{}, errCredentialMutationInvalid
		}
		fields := map[string]bool{"name": true, "role": true, "scopes": true, "agentResources": true, "machineResources": true, "expiresAt": true}
		for field := range policy {
			if !fields[field] {
				return credentialMutationRequest{}, errCredentialMutationInvalid
			}
		}
	}
	var request credentialMutationRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return credentialMutationRequest{}, errCredentialMutationInvalid
	}
	return request, nil
}

func (s *Server) handleOperatorCredentialMutation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "manage-operator-credentials") {
		return
	}
	authority, journal, available := s.credentialManagementDependencies()
	if !available {
		writeCredentialManagementUnavailable(w)
		return
	}
	request, err := decodeCredentialMutationRequest(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_credential_change", "Review the key name, permissions, targets and new value.")
		return
	}
	operationCtx, stop := context.WithTimeout(r.Context(), 8*time.Second)
	defer stop()
	r = r.WithContext(operationCtx)
	snapshot, err := authority.Load(r.Context())
	if err != nil || snapshot.UID == "" || snapshot.Document.Namespace != s.Namespace {
		writeCredentialManagementUnavailable(w)
		return
	}
	if r.Header.Get("If-Match") == "" {
		writeJSONError(w, http.StatusPreconditionRequired, "credential_version_required", "Refresh the key list before making a change.")
		return
	}
	if r.Header.Get("If-Match") != credentialRegistryETag(snapshot.UID, snapshot.Version) {
		writeJSONError(w, http.StatusConflict, "credential_version_changed", "The key list changed. Refresh and review the change again.")
		return
	}
	actor := callerFrom(r.Context())
	if actor == nil {
		writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		return
	}
	request.requestID = w.Header().Get(RequestIDHeader)
	next, operation, err := prepareManagedCredentialMutation(snapshot, *actor, request, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, errCredentialMutationForbidden):
			writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		case errors.Is(err, errCredentialLastAdministrator):
			writeJSONError(w, http.StatusConflict, "last_administrator_required", "Keep at least one administrator key that does not expire.")
		case errors.Is(err, errOperatorAuthorityUnavailable):
			writeCredentialManagementUnavailable(w)
		default:
			writeJSONError(w, http.StatusBadRequest, "invalid_credential_change", "Review the key name, permissions, targets and new value.")
		}
		return
	}
	// A repeated POST never asserts that the new body/material was accepted.
	// Clients look up the original operation after a lost response instead.
	existing, err := journal.GetCredentialOperation(r.Context(), s.Namespace, operation.ID)
	if err == nil {
		writeJSON(w, http.StatusConflict, credentialOperationProjection(existing))
		return
	}
	if !errors.Is(err, securityaudit.ErrOperationNotFound) {
		writeCredentialManagementUnavailable(w)
		return
	}
	admission := securityaudit.Event{ID: operation.AdmissionEventID, OperationID: operation.ID, ObservedAt: operation.At, Namespace: s.Namespace, PrincipalID: operation.ActorPrincipalID, CredentialID: operation.ActorCredentialID, Generation: operation.ActorGeneration, CredentialAuthority: "registry", RequestID: operation.RequestID, Action: "credential:" + operation.Action, ResourceKind: "credential", ResourceName: operation.TargetCredentialID, Phase: "authorization", Outcome: "allow", Method: http.MethodPost}
	intent := securityaudit.CredentialIntent{Admission: admission, CompletionEventID: operation.CompletionEventID, ExpectedVersion: snapshot.Version, RegistryUID: snapshot.UID}
	admitted, err := journal.AdmitCredentialOperation(r.Context(), intent)
	if err != nil {
		// No credential write follows an unacknowledged admission. Its commit may
		// still become visible, so report a checkable pending operation, not failure.
		if errors.Is(err, securityaudit.ErrConflict) {
			writeJSONError(w, http.StatusConflict, "credential_operation_recorded", "This operation is already recorded. Check its outcome.")
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "credential_admission_unconfirmed", "The change could not be admitted with certainty. Check this operation before retrying.")
		return
	}
	if !admitted.NewlyAdmitted {
		writeJSON(w, http.StatusConflict, credentialOperationProjection(admitted))
		return
	}
	// Reauthorize through the current selected document after admission. The
	// selected writer pins that same UID/version at its own authoritative read.
	current, err := authority.Load(r.Context())
	if err == nil && current.UID == snapshot.UID && current.Version == snapshot.Version {
		_, _, err = prepareManagedCredentialMutation(current, *actor, request, operation.At)
	}
	if err == nil && (current.UID != snapshot.UID || current.Version != snapshot.Version) {
		err = operatorcredentials.ErrConflict
	}
	if err == nil {
		_, err = authority.Replace(r.Context(), snapshot.UID, snapshot.Version, next)
	}
	outcome := ""
	switch {
	case err == nil:
		outcome = "success"
	case errors.Is(err, operatorcredentials.ErrConflict), errors.Is(err, operatorcredentials.ErrInvalid), errors.Is(err, errCredentialMutationForbidden):
		outcome = "failure"
	}
	if outcome == "success" {
		s.refreshManagedRotationSession(w, r, request, *actor)
	}
	// Disconnection or self-rotation must not cancel the record of a confirmed
	// effect. Unknown writes remain pending for authority/outbox reconciliation.
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), securityAuditTimeout)
	defer cancel()
	if outcome != "" {
		if completed, finishErr := journal.FinishCredentialOperation(completionCtx, s.Namespace, operation.ID, outcome, time.Now()); finishErr == nil {
			admitted.Completion = &completed
		}
	}
	status := http.StatusAccepted
	if admitted.Completion != nil {
		status = http.StatusOK
	}
	writeJSON(w, status, credentialOperationProjection(admitted))
}

func (s *Server) handleOperatorCredentialOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "read-credential-operation") {
		return
	}
	authority, journal, available := s.credentialManagementDependencies()
	if !available {
		writeCredentialManagementUnavailable(w)
		return
	}
	operationID := r.URL.Query().Get("operationId")
	parsed, err := uuid.Parse(operationID)
	if err != nil || parsed.String() != operationID {
		writeJSONError(w, http.StatusBadRequest, "invalid_operation", "A valid operation ID is required.")
		return
	}
	lookupCtx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	operation, err := journal.GetCredentialOperation(lookupCtx, s.Namespace, operationID)
	if errors.Is(err, securityaudit.ErrOperationNotFound) {
		writeJSONError(w, http.StatusNotFound, "operation_not_found", "This operation has not been observed. Its outcome is not confirmed.")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "credential_operation_unavailable", "The operation outcome could not be checked.")
		return
	}
	current, currentErr := authority.Load(r.Context())
	actor := callerFrom(r.Context())
	if currentErr != nil || current.Document.Namespace != s.Namespace {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	registry, currentErr := compileRegistryCredentials(current.Document, time.Now())
	if currentErr != nil {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	entry, ok := registry.byID[actor.CredentialID]
	if !ok || !entry.caller.Scopes.full || !sameCredentialIdentity(*actor, entry.caller) {
		writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		return
	}
	writeJSON(w, http.StatusOK, credentialOperationProjection(operation))
}

// Explicit recovery can fence an interrupted attempt. GET remains read-only:
// merely polling an outcome must not cancel an in-flight key change.
func (s *Server) handleReconcileOperatorCredentialOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "resolve-credential-operation") {
		return
	}
	authority, journal, available := s.credentialManagementDependencies()
	if !available {
		writeJSONError(w, http.StatusServiceUnavailable, "credential_recovery_unavailable", "The change could not be checked. Try again.")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.UseNumber()
	var value any
	decodeErr := decodeUniqueJSONValue(decoder, &value, 0)
	object, ok := value.(map[string]any)
	id, _ := object["operationId"].(string)
	parsed, parseErr := uuid.Parse(id)
	_, tailErr := decoder.Token()
	if decodeErr != nil || !ok || len(object) != 1 || parseErr != nil || parsed.String() != id || tailErr != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid_operation", "A valid operation ID is required.")
		return
	}
	recoveryCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	r = r.WithContext(recoveryCtx)
	operation, err := journal.GetCredentialOperation(recoveryCtx, s.Namespace, id)
	if errors.Is(err, securityaudit.ErrOperationNotFound) {
		writeJSONError(w, http.StatusNotFound, "operation_not_found", "This operation has not been observed. Its outcome is not confirmed.")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "credential_recovery_unavailable", "The change could not be checked. Try again.")
		return
	}
	if !s.requireManagementAudit(w, r, "credential:recover", "credential", operation.Intent.Admission.ResourceName, "", true) {
		return
	}
	if operation.Completion != nil {
		completeManagementMutation(r, "credential:recover")
		writeJSON(w, http.StatusOK, credentialOperationProjection(operation))
		return
	}
	beginManagementMutation(r)
	current, err := authority.Load(recoveryCtx)
	if err == nil {
		err = currentManagedAdministrator(current, callerFrom(r.Context()))
	}
	if errors.Is(err, errCredentialMutationForbidden) {
		writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		return
	}
	outcome, err := credentialRecoveryEvidence(operation.Intent, current, err)
	if err == nil && outcome == "" {
		// A successful fence preserves data and advances the original version.
		// A lost acknowledgement or a conflict is resolved by reading, never by
		// issuing a replacement key or assuming the old write failed.
		_, _ = authority.Fence(recoveryCtx, operation.Intent.RegistryUID, operation.Intent.ExpectedVersion)
		current, err = authority.Load(recoveryCtx)
		if err == nil {
			err = currentManagedAdministrator(current, callerFrom(r.Context()))
		}
		if errors.Is(err, errCredentialMutationForbidden) {
			writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
			return
		}
		outcome, err = credentialRecoveryEvidence(operation.Intent, current, err)
	}
	if err == nil && outcome != "" {
		completed, finishErr := journal.FinishCredentialOperation(recoveryCtx, s.Namespace, id, outcome, time.Now())
		if finishErr == nil {
			operation.Completion = &completed
		} else if latest, lookupErr := journal.GetCredentialOperation(recoveryCtx, s.Namespace, id); lookupErr == nil && latest.Completion != nil {
			operation = latest
		}
	}
	status := http.StatusAccepted
	if operation.Completion != nil {
		status = http.StatusOK
		completeManagementMutation(r, "credential:recover")
	}
	writeJSON(w, status, credentialOperationProjection(operation))
}

func currentManagedAdministrator(snapshot operatorcredentials.Snapshot, actor *Caller) error {
	if actor == nil || actor.CredentialAuthority != "registry" {
		return errCredentialMutationForbidden
	}
	registry, err := compileRegistryCredentials(snapshot.Document, time.Now())
	if err != nil {
		return errOperatorAuthorityUnavailable
	}
	entry, ok := registry.byID[actor.CredentialID]
	if !ok || !entry.caller.Scopes.full || !sameCredentialIdentity(*actor, entry.caller) {
		return errCredentialMutationForbidden
	}
	return nil
}

func credentialRecoveryEvidence(intent securityaudit.CredentialIntent, current operatorcredentials.Snapshot, loadErr error) (string, error) {
	if loadErr != nil || current.UID == "" || current.Version == "" || current.UID != intent.RegistryUID || current.Document.Namespace != intent.Admission.Namespace {
		return "", errOperatorAuthorityUnavailable
	}
	if _, err := compileRegistryCredentials(current.Document, time.Now()); err != nil {
		return "", errOperatorAuthorityUnavailable
	}
	for _, reference := range current.Document.Operations {
		if reference.ID != intent.Admission.OperationID {
			continue
		}
		if reference.AdmissionEventID != intent.Admission.ID || reference.CompletionEventID != intent.CompletionEventID || "credential:"+reference.Action != intent.Admission.Action || reference.ActorPrincipalID != intent.Admission.PrincipalID || reference.ActorCredentialID != intent.Admission.CredentialID || reference.ActorGeneration != intent.Admission.Generation || reference.TargetCredentialID != intent.Admission.ResourceName || reference.RequestID != intent.Admission.RequestID || reference.Authority != "registry" || !reference.At.Equal(intent.Admission.ObservedAt) {
			return "", errOperatorAuthorityUnavailable
		}
		return "success", nil
	}
	if current.Version != intent.ExpectedVersion {
		return "failure", nil
	}
	return "", nil
}
