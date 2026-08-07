package api

import (
	"log/slog"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// FleetSummaryResponse is the JSON response for GET /api/v1/fleet/summary.
type FleetSummaryResponse struct {
	MachineCount    int            `json:"machineCount"`
	AgentCount      int            `json:"agentCount"`
	AgentsByPhase   map[string]int `json:"agentsByPhase"`
	MachinesByPhase map[string]int `json:"machinesByPhase"`
}

// handleFleet dispatches /api/v1/fleet and /api/v1/fleet/summary.
// The spec defines GET /api/v1/fleet as the fleet overview endpoint; the task
// description also references /api/v1/fleet/summary. Both are served here.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	// Accept both /api/v1/fleet and /api/v1/fleet/summary.
	path := r.URL.Path
	if path != "/api/v1/fleet" && path != "/api/v1/fleet/summary" {
		writeJSONError(w, http.StatusNotFound, "not_found", "not found")
		return
	}

	agents := &kyberv1.AgentList{}
	if err := s.K8sClient.List(r.Context(), agents, client.InNamespace(s.Namespace)); err != nil {
		slog.Error("failed to list agents for fleet summary", "namespace", s.Namespace, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list agents")
		return
	}

	machines := &kyberv1.MachineList{}
	if err := s.K8sClient.List(r.Context(), machines, client.InNamespace(s.Namespace)); err != nil {
		slog.Error("failed to list machines for fleet summary", "namespace", s.Namespace, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list machines")
		return
	}

	agentsByPhase := make(map[string]int)
	for i := range agents.Items {
		phase := string(agents.Items[i].Status.Phase)
		if phase == "" {
			phase = "Unknown"
		}
		agentsByPhase[phase]++
	}

	machinesByPhase := make(map[string]int)
	for i := range machines.Items {
		phase := string(machines.Items[i].Status.Phase)
		if phase == "" {
			phase = "Unknown"
		}
		machinesByPhase[phase]++
	}

	writeJSON(w, http.StatusOK, FleetSummaryResponse{
		MachineCount:    len(machines.Items),
		AgentCount:      len(agents.Items),
		AgentsByPhase:   agentsByPhase,
		MachinesByPhase: machinesByPhase,
	})
}
