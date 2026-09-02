package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/capabilities"
)

type agentCapabilitiesResponse struct {
	SchemaVersion   string                          `json:"schemaVersion"`
	AgentResourceID string                          `json:"agentResourceId"`
	Revision        string                          `json:"revision"`
	GeneratedAt     string                          `json:"generatedAt"`
	Identity        capabilities.PublicIdentity     `json:"identity"`
	Capabilities    []agentPublicCapabilityResponse `json:"capabilities"`
}

type agentPublicCapabilityResponse struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	InputModes   []string `json:"inputModes"`
	OutputModes  []string `json:"outputModes"`
	TaskFeatures []string `json:"taskFeatures,omitempty"`
	Availability string   `json:"availability"`
	Reason       string   `json:"reason,omitempty"`
}

func (s *Server) handleAgentCapabilities(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !s.requireCapabilityResource(w, r, name, ScopeCapabilitiesRead) {
		return
	}
	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent capabilities not found")
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "agent_unavailable", "agent capabilities unavailable")
		return
	}
	if agent.Spec.PublicCapabilities == nil {
		writeJSONError(w, http.StatusNotFound, "not_declared", "agent does not declare public capabilities")
		return
	}
	manifest, digest, err := capabilities.NormalizeAndValidate(agent.Spec.PublicCapabilities)
	if err != nil {
		writeJSONError(w, http.StatusConflict, "invalid_manifest", "agent capability manifest is invalid")
		return
	}
	status := agent.Status.PublicCapabilities
	if status == nil || status.ObservedGeneration != agent.Generation || status.ManifestRevision != digest {
		writeJSONError(w, http.StatusServiceUnavailable, "manifest_pending", "agent capability manifest has not been reconciled")
		return
	}
	valid := meta.FindStatusCondition(status.Conditions, "Valid")
	if valid == nil || valid.Status != "True" {
		writeJSONError(w, http.StatusConflict, "invalid_manifest", "agent capability manifest is invalid")
		return
	}
	availability := make(map[string]kyberv1.AgentPublicCapabilityAvailability, len(status.Capabilities))
	for _, item := range status.Capabilities {
		availability[item.ID] = item
	}
	response := agentCapabilitiesResponse{
		SchemaVersion: manifest.SchemaVersion, AgentResourceID: string(agent.UID), Revision: digest,
		Identity: manifest.Identity, Capabilities: make([]agentPublicCapabilityResponse, 0, len(manifest.Capabilities)),
	}
	if status.ObservedAt != nil {
		response.GeneratedAt = status.ObservedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	for _, capability := range manifest.Capabilities {
		state := availability[capability.ID]
		if state.Availability == "" {
			state.Availability, state.Reason = "unknown", "status-missing"
		}
		response.Capabilities = append(response.Capabilities, agentPublicCapabilityResponse{
			ID: capability.ID, Version: capability.Version, Name: capability.Name, Description: capability.Description,
			InputModes: capability.InputModes, OutputModes: capability.OutputModes, TaskFeatures: capability.TaskFeatures,
			Availability: state.Availability, Reason: state.Reason,
		})
	}
	encoded, _ := json.Marshal(response)
	sum := sha256.Sum256(encoded)
	etag := `"sha256:` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=10, must-revalidate")
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) requireCapabilityResource(w http.ResponseWriter, r *http.Request, name string, scope Scope) bool {
	if !s.requireScope(w, r, name, "capabilities-access", scope) {
		return false
	}
	caller := callerFrom(r.Context())
	if caller == nil || caller.PrincipalID == "" || caller.TenantID == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "stable principal identity required")
		return false
	}
	if !caller.AgentResources.Has(s.Namespace + "/" + name) {
		writeJSONError(w, http.StatusNotFound, "not_found", "agent capabilities not found")
		return false
	}
	return true
}
