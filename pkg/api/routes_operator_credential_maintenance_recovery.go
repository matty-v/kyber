package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type operatorCredentialMaintenanceManager interface {
	OperatorCredentialMaintenanceReader
	WriterState(context.Context, string, string) (string, error)
	PrepareRecovery(context.Context, string, string, string, string) (operatorcredentials.MaintenanceRecoveryHandle, error)
	ResumeRecovery(context.Context, string, operatorcredentials.MaintenanceReleaseProof) (operatorcredentials.MaintenanceRecoveryHandle, error)
}
type OperatorCredentialMaintenanceReconciler interface {
	Namespace() string
	Verify(context.Context, operatorcredentials.MaintenanceStatus) error
}
type maintenanceRecoveryOperationView struct {
	OperationID string `json:"operationId"`
	Status      string `json:"status"`
	ObservedAt  string `json:"observedAt"`
}

func maintenanceRecoveryProjection(operation securityaudit.CredentialOperation) maintenanceRecoveryOperationView {
	result := maintenanceRecoveryOperationView{OperationID: operation.Intent.Admission.OperationID, Status: "pending", ObservedAt: operation.Intent.Admission.ObservedAt.UTC().Format(time.RFC3339Nano)}
	if operation.Completion != nil {
		result.Status = operation.Completion.Outcome
		result.ObservedAt = operation.Completion.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	return result
}
func (s *Server) maintenanceRecoveryDependencies() (operatorCredentialMaintenanceManager, securityaudit.CredentialMaintenanceJournal, OperatorCredentialMaintenanceReconciler, bool) {
	manager, managed := s.OperatorCredentialMaintenance.(operatorCredentialMaintenanceManager)
	journal, journaled := s.SecurityAudit.(securityaudit.CredentialMaintenanceJournal)
	verifier := s.OperatorCredentialMaintenanceReconciler
	return manager, journal, verifier, managed && journaled && verifier != nil && manager.Namespace() == s.Namespace && verifier.Namespace() == s.Namespace
}
func (s *Server) currentMaintenanceAdministrator(r *http.Request) error {
	actor := callerFrom(r.Context())
	current, err := s.auth.Authenticate(r)
	if errors.Is(err, errOperatorAuthorityUnavailable) {
		return err
	}
	if err != nil || actor == nil || current == nil || !current.Scopes.full || !sameCredentialIdentity(*current, *actor) {
		return errCredentialMutationForbidden
	}
	return nil
}
func writeMaintenanceRecoveryUnavailable(w http.ResponseWriter) {
	writeJSONError(w, http.StatusServiceUnavailable, "maintenance_recovery_unavailable", "Recovery could not be verified. Check the operation before retrying.")
}
func (s *Server) checkMaintenanceActor(w http.ResponseWriter, r *http.Request) bool {
	err := s.currentMaintenanceAdministrator(r)
	if errors.Is(err, errOperatorAuthorityUnavailable) {
		writeOperatorAuthenticationUnavailable(w)
		return false
	}
	if err != nil {
		writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		return false
	}
	return true
}
func decodeMaintenanceOperationID(w http.ResponseWriter, r *http.Request) (string, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.UseNumber()
	var value any
	err := decodeUniqueJSONValue(decoder, &value, 0)
	object, ok := value.(map[string]any)
	id, _ := object["operationId"].(string)
	parsed, parseErr := uuid.Parse(id)
	_, tailErr := decoder.Token()
	if err != nil || !ok || len(object) != 1 || parseErr != nil || parsed == uuid.Nil || parsed.String() != id || tailErr != io.EOF {
		return "", operatorcredentials.ErrInvalid
	}
	return id, nil
}
func validMaintenanceOperation(operation securityaudit.CredentialOperation) bool {
	return operation.Intent.Validate() == nil && operation.Intent.Admission.ResourceKind == "credential-maintenance"
}

// Eligibility is read-only. A different pending recovery cannot be displaced.
// Replacement is permitted only after its immutable failure and a fenced effect.
func (s *Server) maintenanceRecoveryEligibility(ctx context.Context, manager operatorCredentialMaintenanceManager, journal securityaudit.CredentialMaintenanceJournal, verifier OperatorCredentialMaintenanceReconciler, status operatorcredentials.MaintenanceStatus, operationID string) (string, string) {
	if !status.Present {
		return "idle", ""
	}
	if status.Kind != operatorcredentials.MaintenanceUpgrade || status.Writer == nil {
		return "writer-unverified", ""
	}
	replacing := ""
	if previous := status.RecoveryOperationID; previous != "" && previous != operationID {
		old, err := journal.GetCredentialOperation(ctx, s.Namespace, previous)
		if err != nil || !validMaintenanceOperation(old) || old.Intent.MaintenanceUID != status.UID {
			return "recovery-unverified", ""
		}
		if old.Completion == nil {
			return "recovery-pending", ""
		}
		if old.Completion.Outcome != "failure" || old.Effect != nil && old.Effect.ResourceVersion == status.Version {
			return "recovery-unverified", ""
		}
		replacing = previous
	}
	state, err := manager.WriterState(ctx, status.UID, status.Version)
	if errors.Is(err, operatorcredentials.ErrConflict) {
		return "metadata-changed", ""
	}
	if err != nil || state == operatorcredentials.MaintenanceWriterUnverified {
		return "writer-unverified", ""
	}
	if state != operatorcredentials.MaintenanceWriterStopped {
		return "writer-running", ""
	}
	if verifier.Verify(ctx, status) != nil {
		return "release-unverified", ""
	}
	latest, err := manager.Inspect(ctx)
	if err != nil || !latest.Present || latest.UID != status.UID || latest.Version != status.Version {
		return "metadata-changed", ""
	}
	return "ready", replacing
}
func (s *Server) handleMaintenanceRecoveryReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "check-maintenance-recovery") {
		return
	}
	manager, journal, verifier, ok := s.maintenanceRecoveryDependencies()
	if !ok {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	status, err := manager.Inspect(ctx)
	if err != nil {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	reason, _ := s.maintenanceRecoveryEligibility(ctx, manager, journal, verifier, status, "")
	if !s.checkMaintenanceActor(w, r) {
		return
	}
	version := ""
	if status.Present {
		version = credentialRegistryETag(status.UID, status.Version)
		w.Header().Set("ETag", version)
	}
	writeJSON(w, 200, struct {
		Ready       bool   `json:"ready"`
		Reason      string `json:"reason"`
		Version     string `json:"version,omitempty"`
		OperationID string `json:"operationId,omitempty"`
	}{reason == "ready", reason, version, status.RecoveryOperationID})
}
func (s *Server) handleRecoverOperatorCredentialMaintenance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeJSONError(w, 405, "method_not_allowed", "POST required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "recover-credential-maintenance") {
		return
	}
	manager, journal, verifier, ok := s.maintenanceRecoveryDependencies()
	if !ok {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	id, err := decodeMaintenanceOperationID(w, r)
	if err != nil {
		writeJSONError(w, 400, "invalid_operation", "A valid operation ID is required.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	existing, err := journal.GetCredentialOperation(ctx, s.Namespace, id)
	if err == nil {
		if !validMaintenanceOperation(existing) {
			writeJSONError(w, 409, "operation_recorded", "This operation ID is already used.")
			return
		}
		if !s.checkMaintenanceActor(w, r) {
			return
		}
		s.writeMaintenanceOperation(w, existing)
		return
	}
	if !errors.Is(err, securityaudit.ErrOperationNotFound) {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	if r.Header.Get("If-Match") == "" {
		writeJSONError(w, 428, "maintenance_version_required", "Refresh maintenance status before recovery.")
		return
	}
	status, err := manager.Inspect(ctx)
	if err != nil {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	if !status.Present || r.Header.Get("If-Match") != credentialRegistryETag(status.UID, status.Version) {
		writeJSONError(w, 409, "maintenance_changed", "Maintenance changed. Refresh and check recovery again.")
		return
	}
	reason, _ := s.maintenanceRecoveryEligibility(ctx, manager, journal, verifier, status, id)
	if reason != "ready" {
		writeJSONError(w, 409, "maintenance_not_ready", "Recovery is not verified. Check maintenance status.")
		return
	}
	if !s.checkMaintenanceActor(w, r) {
		return
	}
	actor := callerFrom(r.Context())
	authority := actor.CredentialAuthority
	if authority == "" {
		authority = "configuration"
	}
	admission := securityaudit.Event{ID: uuid.NewString(), OperationID: id, ObservedAt: time.Now().UTC(), Namespace: s.Namespace, PrincipalID: actor.PrincipalID, CredentialID: actor.CredentialID, Generation: actor.CredentialGeneration, CredentialAuthority: authority, RequestID: w.Header().Get(RequestIDHeader), Action: "credential:recover-maintenance", ResourceKind: "credential-maintenance", ResourceName: status.ExecutionID, Phase: "authorization", Outcome: "allow", Method: http.MethodPost}
	intent := securityaudit.CredentialIntent{Admission: admission, CompletionEventID: uuid.NewString(), ExpectedVersion: status.Version, MaintenanceUID: status.UID}
	operation, err := journal.AdmitCredentialOperation(ctx, intent)
	if err != nil {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	if !operation.NewlyAdmitted {
		if !s.checkMaintenanceActor(w, r) {
			return
		}
		s.writeMaintenanceOperation(w, operation)
		return
	}
	s.executeMaintenanceRecovery(w, r, manager, journal, verifier, operation)
}
func (s *Server) writeMaintenanceOperation(w http.ResponseWriter, operation securityaudit.CredentialOperation) {
	status := http.StatusAccepted
	if operation.Completion != nil {
		status = http.StatusOK
	}
	writeJSON(w, status, maintenanceRecoveryProjection(operation))
}
func (s *Server) executeMaintenanceRecovery(w http.ResponseWriter, r *http.Request, manager operatorCredentialMaintenanceManager, journal securityaudit.CredentialMaintenanceJournal, verifier OperatorCredentialMaintenanceReconciler, operation securityaudit.CredentialOperation) {
	respond := func() {
		if s.checkMaintenanceActor(w, r) {
			if operation.Completion != nil {
				completeManagementMutation(r, "credential:recover")
			}
			s.writeMaintenanceOperation(w, operation)
		}
	}
	if operation.Completion != nil {
		respond()
		return
	}
	id := operation.Intent.Admission.OperationID
	status, err := manager.Inspect(r.Context())
	if err != nil {
		respond()
		return
	}
	finish := func(outcome string) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), securityAuditTimeout)
		defer cancel()
		if event, err := journal.FinishCredentialMaintenanceOperation(ctx, s.Namespace, id, outcome, time.Now(), operation.Effect); err == nil {
			operation.Completion = &event
		}
	}
	if !status.Present || status.UID != operation.Intent.MaintenanceUID {
		if operation.Effect != nil {
			finish("success")
		} else {
			finish("failure")
		}
		respond()
		return
	}
	if operation.Effect != nil && status.Version != operation.Effect.ResourceVersion {
		finish("failure")
		respond()
		return
	}
	var capability operatorcredentials.MaintenanceRecoveryHandle
	if operation.Effect == nil {
		if status.Version != operation.Intent.ExpectedVersion && status.RecoveryOperationID != id {
			finish("failure")
			respond()
			return
		}
		reason, replacing := s.maintenanceRecoveryEligibility(r.Context(), manager, journal, verifier, status, id)
		if reason != "ready" {
			respond()
			return
		}
		if !s.checkMaintenanceActor(w, r) {
			return
		}
		beginManagementMutation(r)
		capability, err = manager.PrepareRecovery(r.Context(), status.UID, status.Version, id, replacing)
		if err != nil || capability == nil {
			respond()
			return
		}
		proof := capability.Proof()
		effect := securityaudit.CredentialMaintenanceEffect{ResourceUID: proof.ResourceUID, ResourceVersion: proof.ResourceVersion, ProofID: proof.ProofID}
		if effect.ResourceUID != operation.Intent.MaintenanceUID || effect.ResourceVersion == operation.Intent.ExpectedVersion {
			respond()
			return
		}
		updated, err := journal.RecordCredentialMaintenanceEffect(r.Context(), s.Namespace, id, effect)
		if err != nil || !validMaintenanceOperation(updated) || updated.Intent != operation.Intent || updated.Effect == nil || *updated.Effect != effect {
			respond()
			return
		}
		operation = updated
		if operation.Completion != nil {
			respond()
			return
		}
	} else {
		reason, _ := s.maintenanceRecoveryEligibility(r.Context(), manager, journal, verifier, status, id)
		if reason != "ready" {
			respond()
			return
		}
		proof := operation.Effect
		capability, err = manager.ResumeRecovery(r.Context(), id, operatorcredentials.MaintenanceReleaseProof{ResourceUID: proof.ResourceUID, ResourceVersion: proof.ResourceVersion, ProofID: proof.ProofID})
		if err != nil {
			respond()
			return
		}
	}
	if !s.checkMaintenanceActor(w, r) {
		return
	}
	if capability == nil {
		respond()
		return
	}
	beginManagementMutation(r)
	if err := capability.Release(r.Context()); err == nil {
		after, readErr := manager.Inspect(r.Context())
		if readErr == nil && (!after.Present || after.UID != operation.Intent.MaintenanceUID) {
			finish("success")
		}
	}
	respond()
}
func (s *Server) handleMaintenanceRecoveryOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "read-maintenance-recovery") {
		return
	}
	_, journal, _, ok := s.maintenanceRecoveryDependencies()
	if !ok {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	id := r.URL.Query().Get("operationId")
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		writeJSONError(w, 400, "invalid_operation", "A valid operation ID is required.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	operation, err := journal.GetCredentialOperation(ctx, s.Namespace, id)
	if errors.Is(err, securityaudit.ErrOperationNotFound) {
		writeJSONError(w, 404, "operation_not_found", "This operation has not been observed. Its outcome is not confirmed.")
		return
	}
	if err != nil || !validMaintenanceOperation(operation) {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	if s.checkMaintenanceActor(w, r) {
		writeJSON(w, 200, maintenanceRecoveryProjection(operation))
	}
}
func (s *Server) handleResolveMaintenanceRecovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeJSONError(w, 405, "method_not_allowed", "POST required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "resolve-maintenance-recovery") {
		return
	}
	manager, journal, verifier, ok := s.maintenanceRecoveryDependencies()
	if !ok {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	id, err := decodeMaintenanceOperationID(w, r)
	if err != nil {
		writeJSONError(w, 400, "invalid_operation", "A valid operation ID is required.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	operation, err := journal.GetCredentialOperation(ctx, s.Namespace, id)
	if err != nil || !validMaintenanceOperation(operation) {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	if operation.Completion != nil {
		if s.checkMaintenanceActor(w, r) {
			s.writeMaintenanceOperation(w, operation)
		}
		return
	}
	if !s.requireManagementAudit(w, r, "credential:recover", "credential-maintenance", operation.Intent.Admission.ResourceName, "", true) {
		return
	}
	s.executeMaintenanceRecovery(w, r, manager, journal, verifier, operation)
}

func (s *Server) handlePendingMaintenanceRecoveryOperations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "list-pending-maintenance-recovery") {
		return
	}
	_, journal, _, ok := s.maintenanceRecoveryDependencies()
	if !ok {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	after := r.URL.Query().Get("after")
	if after != "" {
		id, err := uuid.Parse(after)
		if err != nil || id == uuid.Nil || id.String() != after {
			writeJSONError(w, 400, "invalid_cursor", "A valid continuation is required.")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	operations, err := journal.PendingCredentialMaintenanceOperations(ctx, s.Namespace, after, 25)
	if err != nil {
		writeMaintenanceRecoveryUnavailable(w)
		return
	}
	items := []maintenanceRecoveryOperationView{}
	next := ""
	for _, operation := range operations {
		if !validMaintenanceOperation(operation) || operation.Intent.Admission.Namespace != s.Namespace {
			writeMaintenanceRecoveryUnavailable(w)
			return
		}
		items = append(items, maintenanceRecoveryProjection(operation))
	}
	if len(items) == 25 {
		next = items[len(items)-1].OperationID
	}
	if !s.checkMaintenanceActor(w, r) {
		return
	}
	writeJSON(w, 200, struct {
		Items []maintenanceRecoveryOperationView `json:"items"`
		Next  string                             `json:"next,omitempty"`
	}{items, next})
}
