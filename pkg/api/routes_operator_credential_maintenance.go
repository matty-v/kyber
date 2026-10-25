package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

type OperatorCredentialMaintenanceReader interface {
	Inspect(context.Context) (operatorcredentials.MaintenanceStatus, error)
	Namespace() string
}

type credentialMaintenanceView struct {
	RecoveryAvailable   bool                                `json:"recoveryAvailable,omitempty"`
	RecoveryOperationID string                              `json:"recoveryOperationId,omitempty"`
	Writer              *credentialMaintenanceWriterView    `json:"writer,omitempty"`
	ObservedAt          string                              `json:"observedAt"`
	Blocked             bool                                `json:"blocked"`
	Version             string                              `json:"version,omitempty"`
	Operation           *credentialMaintenanceOperationView `json:"operation,omitempty"`
}
type credentialMaintenanceWriterView struct {
	PodName string `json:"podName"`
	JobName string `json:"jobName"`
}

type credentialMaintenanceOperationView struct {
	Kind        string `json:"kind"`
	ExecutionID string `json:"executionId"`
	StartedAt   string `json:"startedAt"`
}

func (s *Server) credentialMaintenanceAvailable() bool {
	return s.OperatorCredentialMaintenance != nil && s.OperatorCredentialMaintenance.Namespace() == s.Namespace
}

// Inspection never acquires, releases or changes a maintenance reservation.
func (s *Server) handleOperatorCredentialMaintenance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "inspect-credential-maintenance") {
		return
	}
	if !s.credentialMaintenanceAvailable() {
		writeJSONError(w, http.StatusServiceUnavailable, "credential_maintenance_unavailable", "Maintenance status is unavailable.")
		return
	}
	actor := callerFrom(r.Context())
	if actor == nil {
		writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	status, err := s.OperatorCredentialMaintenance.Inspect(ctx)
	cancel()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "credential_maintenance_unavailable", "Maintenance status could not be checked. No recovery was started.")
		return
	}
	current, err := s.auth.Authenticate(r)
	if errors.Is(err, errOperatorAuthorityUnavailable) {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	if err != nil || current == nil || !current.Scopes.full || !sameCredentialIdentity(*current, *actor) {
		writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
		return
	}
	_, _, _, recoveryAvailable := s.maintenanceRecoveryDependencies()
	response := credentialMaintenanceView{RecoveryAvailable: recoveryAvailable, RecoveryOperationID: status.RecoveryOperationID, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Blocked: status.Present}
	if status.Present {
		if status.Writer != nil {
			response.Writer = &credentialMaintenanceWriterView{PodName: status.Writer.PodName, JobName: status.Writer.JobName}
		}
		if status.UID == "" || status.Version == "" || (status.Kind != operatorcredentials.MaintenanceAdoption && status.Kind != operatorcredentials.MaintenanceUpgrade) || status.ExecutionID == "" || status.StartedAt.IsZero() {
			writeJSONError(w, http.StatusServiceUnavailable, "credential_maintenance_unavailable", "Maintenance status could not be verified.")
			return
		}
		response.Version = credentialRegistryETag(status.UID, status.Version)
		response.Operation = &credentialMaintenanceOperationView{Kind: status.Kind, ExecutionID: status.ExecutionID, StartedAt: status.StartedAt.UTC().Format(time.RFC3339Nano)}
		w.Header().Set("ETag", response.Version)
	}
	writeJSON(w, http.StatusOK, response)
}
