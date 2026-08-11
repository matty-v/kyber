package api

import (
	"net/http"
)

// handleConfigExport serves GET /api/v1/config/export — the values file that
// recreates this cluster, with secrets removed.
//
// This is the infra-as-code artifact for an install that has no deploy repo:
// the cluster owns its config, and this is how an operator gets it back out
// and into version control. See dave-agent spec
// 2026-08-10-kyber-owns-its-deployment.md §5/§9.
//
// A 200 with available:false is the correct answer for an install that is not
// a Helm release — every ArgoCD-managed cluster today. The body then explains
// where the config actually lives rather than returning an error the operator
// cannot act on.
func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	if s.ConfigExporter == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "CONFIG_EXPORT_NOT_CONFIGURED",
			"Config export is not enabled on this control plane.")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			"only GET is supported on /api/v1/config/export")
		return
	}

	export, err := s.ConfigExporter.Load(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "CONFIG_EXPORT_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, export)
}
