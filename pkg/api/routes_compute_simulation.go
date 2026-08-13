package api

import (
	"encoding/json"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

type computeScenarioRequest struct {
	Machine  string                      `json:"machine,omitempty"`
	Scenario adapters.SimulationScenario `json:"scenario"`
}

func (s *Server) handleComputeSimulation(w http.ResponseWriter, r *http.Request) {
	if s.ComputeSimulation == nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.URL.Path == "/api/v1/dev/compute/instances" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"instances": s.ComputeSimulation.ListSimulatedInstances()})
	case r.URL.Path == "/api/v1/dev/compute/scenarios" && r.Method == http.MethodPost:
		var req computeScenarioRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "invalid scenario request")
			return
		}
		if req.Scenario == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "scenario is required")
			return
		}
		if err := s.ComputeSimulation.ApplySimulationScenario(req.Machine, req.Scenario); err != nil {
			writeJSONError(w, http.StatusBadRequest, "scenario_rejected", err.Error())
			return
		}
		if req.Machine != "" && s.K8sClient != nil {
			machine := &kyberv1.Machine{}
			key := client.ObjectKey{Namespace: s.Namespace, Name: req.Machine}
			if err := s.K8sClient.Get(r.Context(), key, machine); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "reconcile_trigger_failed", "scenario applied but Machine reconcile could not be triggered")
				return
			}
			before := machine.DeepCopy()
			if machine.Annotations == nil {
				machine.Annotations = map[string]string{}
			}
			machine.Annotations["kyber.io/dev-scenario-revision"] = time.Now().UTC().Format(time.RFC3339Nano)
			if err := s.K8sClient.Patch(r.Context(), machine, client.MergeFrom(before)); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "reconcile_trigger_failed", "scenario applied but Machine reconcile could not be triggered")
				return
			}
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"machine": req.Machine, "scenario": req.Scenario})
	default:
		w.Header().Set("Allow", allowedSimulationMethods(r.URL.Path))
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func allowedSimulationMethods(path string) string {
	if path == "/api/v1/dev/compute/instances" {
		return http.MethodGet
	}
	return http.MethodPost
}
