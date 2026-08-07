package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/matty-v/kyber/pkg/metricsstore"
)

// handleNodeRoutes is the dispatcher for all routes under /internal/nodes/.
func (s *InternalServer) handleNodeRoutes(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/internal/nodes/")
	parts := strings.SplitN(suffix, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	// Node routes are node-agent-only (kyber#566): an agent identity must not
	// be able to spoof node resource telemetry.
	if !s.authorizeNodeAgent(w, r) {
		return
	}
	nodeName := parts[0]
	switch parts[1] {
	case "resources":
		s.handleNodeResources(w, r, nodeName)
	default:
		http.NotFound(w, r)
	}
}

// nodeResourcesRequest is the payload for POST /internal/nodes/{name}/resources.
type nodeResourcesRequest struct {
	CPUPercent     float64 `json:"cpuPercent"`
	MemUsedBytes   float64 `json:"memUsedBytes"`
	MemTotalBytes  float64 `json:"memTotalBytes"`
	DiskUsedBytes  float64 `json:"diskUsedBytes"`
	DiskTotalBytes float64 `json:"diskTotalBytes"`
	UpdatedAt      string  `json:"updatedAt"`
}

// handleNodeResources handles POST /internal/nodes/{name}/resources.
// Validates the payload and stores the latest resource sample for the node.
// Returns 200 on success, 400 on validation failure, 503 when NodeStore is not configured.
func (s *InternalServer) handleNodeResources(w http.ResponseWriter, r *http.Request, nodeName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.nodeStore == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	if !validDNS1123(nodeName) {
		http.Error(w, "invalid node name: must conform to DNS-1123", http.StatusBadRequest)
		return
	}

	var req nodeResourcesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	// Input validation.
	if req.CPUPercent < 0 || req.CPUPercent > 100 {
		http.Error(w, "cpuPercent must be in [0, 100]", http.StatusBadRequest)
		return
	}
	if req.MemUsedBytes < 0 || req.MemTotalBytes < 0 {
		http.Error(w, "negative byte count", http.StatusBadRequest)
		return
	}
	if req.DiskUsedBytes < 0 || req.DiskTotalBytes < 0 {
		http.Error(w, "negative byte count", http.StatusBadRequest)
		return
	}

	sample := metricsstore.NodeSample{
		CPUPercent:     req.CPUPercent,
		MemUsedBytes:   req.MemUsedBytes,
		MemTotalBytes:  req.MemTotalBytes,
		DiskUsedBytes:  req.DiskUsedBytes,
		DiskTotalBytes: req.DiskTotalBytes,
		UpdatedAt:      req.UpdatedAt,
	}
	if err := s.nodeStore.PutNode(r.Context(), s.namespace, nodeName, sample); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
