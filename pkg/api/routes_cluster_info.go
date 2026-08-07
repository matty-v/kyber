package api

import (
	"encoding/json"
	"net/http"
)

// clusterInfoResponse is the JSON shape returned by GET /api/v1/cluster-info.
// Capabilities is an append-only list of strings — once a kyber release ships
// a capability, it stays in the array forever, so older PWA builds keep
// matching the names they expect. Add new capabilities here when shipping
// new control-plane API surface that views can feature-detect against.
type clusterInfoResponse struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

// clusterInfoCapabilities is the static list reported by the running binary.
// It describes the API surface this build supports, NOT the chart-rendered
// feature flags. New endpoints get a name here when they ship; renaming or
// removing entries breaks pinned PWA builds.
var clusterInfoCapabilities = []string{
	"agents",
	"machines",
	"shell",
	"inbound",
	"command-palette",
	"activity-stream",
}

func (s *Server) handleClusterInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(clusterInfoResponse{
		Name:         s.ClusterName,
		Version:      s.ChartVersion,
		Capabilities: clusterInfoCapabilities,
	})
}
