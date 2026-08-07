package api

import "net/http"

// VersionResponse is the payload returned by GET /api/v1/version.
// Consumed by the PWA Settings > Diagnostics card AND by Chewie's
// /smoke-test skill — Chewie polls /api/v1/version and compares
// .sha against the merge commit SHA to confirm the cluster has rolled to the
// new image before running smoke checks. Stable contract: SHA is the
// full 40-char git commit hash (not short), BuildDate is RFC3339.
// All fields are strings with "" meaning "not known" — empty-field UX
// is the caller's concern.
//
//   - SHA: full 40-char Git SHA of the control-plane build (populated via
//     -ldflags "-X main.BuildSHA=…" in the Dockerfile's Go build step,
//     sourced from ${{ github.sha }} in build.yml).
//   - BuildDate: RFC3339 timestamp of the build (ldflags, same mechanism).
//   - ChartVersion: user-facing release version. Build-injected into the image
//     via -ldflags "-X main.Version=…" (mirroring SHA), so it converges with
//     the image rollout and can never lag the deployed code (kyber#482). Falls
//     back to the chart-rendered /etc/kyber/chart-version file on dev/local
//     builds where no ldflag was passed; empty when neither is present. (Field
//     name kept for contract stability even though the source is no longer the
//     chart.)
//   - Substrate: KYBER_NAMESPACE as seen by the control-plane at boot
//     (e.g. "kyber-prod", "kyber-preview-pr-12"). The PWA uses this
//     as the primary "where am I connected to" hint.
type VersionResponse struct {
	SHA          string `json:"sha"`
	BuildDate    string `json:"buildDate"`
	ChartVersion string `json:"chartVersion"`
	Substrate    string `json:"substrate"`
}

// handleVersion serves GET /api/v1/version. Behind the standard API-key auth;
// operators who haven't configured a key see "—" for version fields in the
// Diagnostics card and rely on /healthz alone for the Connected indicator.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			"only GET is supported on /api/v1/version")
		return
	}
	writeJSON(w, http.StatusOK, VersionResponse{
		SHA:          s.BuildSHA,
		BuildDate:    s.BuildDate,
		ChartVersion: s.ChartVersion,
		Substrate:    s.Substrate,
	})
}
