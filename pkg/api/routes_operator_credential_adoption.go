package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

func (s *Server) adoptionManager() (operatorCredentialAdoptionManager, bool) {
	manager := s.OperatorCredentialAdoption
	return manager, manager != nil && manager.Namespace() == s.Namespace
}
func decodeAdoptionBody(w http.ResponseWriter, r *http.Request, target any, fields map[string]bool) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2048))
	if err != nil {
		return operatorcredentials.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decodeUniqueJSONValue(decoder, &value, 0) != nil {
		return operatorcredentials.ErrInvalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return operatorcredentials.ErrInvalid
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != len(fields) {
		return operatorcredentials.ErrInvalid
	}
	for key := range object {
		if !fields[key] {
			return operatorcredentials.ErrInvalid
		}
	}
	if json.Unmarshal(raw, target) != nil {
		return operatorcredentials.ErrInvalid
	}
	return nil
}
func writeAdoptionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCredentialMutationForbidden):
		writeJSONError(w, 403, "forbidden", "Current administrator access required")
	case errors.Is(err, errOperatorAuthorityUnavailable):
		writeOperatorAuthenticationUnavailable(w)
	case errors.Is(err, operatorcredentials.ErrInvalid):
		writeJSONError(w, 400, "invalid_credential_setup", "Review and confirm the setup before continuing.")
	case errors.Is(err, operatorcredentials.ErrConflict):
		writeJSONError(w, 409, "credential_setup_changed", "Setup state changed. Refresh before continuing.")
	default:
		writeJSONError(w, 503, "credential_setup_unavailable", "Setup could not be verified. Check its status before trying again.")
	}
}
func (s *Server) handleOperatorCredentialAdoption(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSONError(w, 405, "method_not_allowed", "GET or POST required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "manage-operator-credentials") || !s.checkMaintenanceActor(w, r) {
		return
	}
	manager, available := s.adoptionManager()
	if !available {
		writeAdoptionError(w, operatorcredentials.ErrUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	actor := callerFrom(r.Context())
	if actor == nil {
		writeAdoptionError(w, errCredentialMutationForbidden)
		return
	}
	if r.Method == http.MethodGet {
		plan, err := manager.Prepare(ctx, *actor)
		if !s.checkMaintenanceActor(w, r) {
			return
		}
		if err != nil {
			writeAdoptionError(w, err)
			return
		}
		writeJSON(w, 200, plan)
		return
	}
	var request OperatorCredentialAdoptionStart
	if decodeAdoptionBody(w, r, &request, map[string]bool{"operationId": true, "version": true, "confirmOwnership": true}) != nil {
		writeAdoptionError(w, operatorcredentials.ErrInvalid)
		return
	}
	authorize := func(ctx context.Context) error { return s.currentMaintenanceAdministrator(r.WithContext(ctx)) }
	view, err := manager.Start(ctx, request, *actor, w.Header().Get(RequestIDHeader), authorize)
	if !s.checkMaintenanceActor(w, r) {
		return
	}
	if err != nil {
		writeAdoptionError(w, err)
		return
	}
	writeJSON(w, 202, view)
}
func (s *Server) handleOperatorCredentialAdoptionOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "read-credential-operation") || !s.checkMaintenanceActor(w, r) {
		return
	}
	manager, available := s.adoptionManager()
	if !available {
		writeAdoptionError(w, operatorcredentials.ErrUnavailable)
		return
	}
	id := r.URL.Query().Get("operationId")
	if !canonicalAdoptionUUID(id) {
		writeAdoptionError(w, operatorcredentials.ErrInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	view, err := manager.Outcome(ctx, id)
	if !s.checkMaintenanceActor(w, r) {
		return
	}
	if err != nil {
		writeAdoptionError(w, err)
		return
	}
	writeJSON(w, 200, view)
}
func (s *Server) handlePendingOperatorCredentialAdoptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "read-credential-operation") || !s.checkMaintenanceActor(w, r) {
		return
	}
	manager, available := s.adoptionManager()
	if !available {
		writeAdoptionError(w, operatorcredentials.ErrUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	views, err := manager.Pending(ctx)
	if !s.checkMaintenanceActor(w, r) {
		return
	}
	if err != nil {
		writeAdoptionError(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Items []OperatorCredentialAdoptionOperation `json:"items"`
		Limit int                                   `json:"limit"`
	}{views, 25})
}
func (s *Server) handleRecoverOperatorCredentialAdoption(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeJSONError(w, 405, "method_not_allowed", "POST required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "recover-credential-operation") || !s.checkMaintenanceActor(w, r) {
		return
	}
	manager, available := s.adoptionManager()
	if !available {
		writeAdoptionError(w, operatorcredentials.ErrUnavailable)
		return
	}
	var request OperatorCredentialAdoptionRecovery
	if decodeAdoptionBody(w, r, &request, map[string]bool{"operationId": true, "attemptId": true}) != nil {
		writeAdoptionError(w, operatorcredentials.ErrInvalid)
		return
	}
	actor := callerFrom(r.Context())
	if actor == nil {
		writeAdoptionError(w, errCredentialMutationForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	authorize := func(ctx context.Context) error { return s.currentMaintenanceAdministrator(r.WithContext(ctx)) }
	view, err := manager.Recover(ctx, request, *actor, w.Header().Get(RequestIDHeader), authorize)
	if !s.checkMaintenanceActor(w, r) {
		return
	}
	if err != nil {
		writeAdoptionError(w, err)
		return
	}
	writeJSON(w, 202, view)
}
