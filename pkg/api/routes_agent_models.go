package api

import (
	"errors"
	"net/http"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

type agentModelsResponse struct {
	Models []AvailableModel `json:"models"`
}

// handleAgentModels returns only the catalog reported by this agent's
// authenticated runtime. Provider credentials never leave the pod or reach the
// browser; the runtime publishes non-secret model metadata through its sidecar.
func (s *Server) handleAgentModels(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
	var agent kyberv1.Agent
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read agent")
		return
	}
	if s.RuntimeDetectCache == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "catalog_unavailable", "runtime catalog storage is not configured")
		return
	}
	if catalogs, ok := s.RuntimeDetectCache.(runtimedetect.AgentCatalogCache); ok {
		models, err := catalogs.GetAgentModels(r.Context(), name)
		if err == nil && len(models) > 0 {
			writeJSON(w, http.StatusOK, agentModelsResponse{Models: availableModels(models)})
			return
		}
		if errors.Is(err, runtimedetect.ErrCacheEmpty) || len(models) == 0 {
			writeJSONError(w, http.StatusConflict, "authentication_required", "the agent has not reported an authenticated model catalog yet")
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "catalog_unavailable", "runtime catalog storage is unavailable")
		return
	}
	snap, err := s.RuntimeDetectCache.Get(r.Context())
	if err != nil {
		if errors.Is(err, runtimedetect.ErrCacheEmpty) {
			writeJSONError(w, http.StatusConflict, "authentication_required", "the agent has not reported an authenticated model catalog yet")
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "catalog_unavailable", "runtime catalog storage is unavailable")
		return
	}
	models, ok := snap.AgentModels[name]
	if !ok || len(models) == 0 {
		writeJSONError(w, http.StatusConflict, "authentication_required", "the agent has not reported an authenticated model catalog yet")
		return
	}
	writeJSON(w, http.StatusOK, agentModelsResponse{Models: availableModels(models)})
}
