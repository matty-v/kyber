// Package api / GET /api/v1/available — surfaces the detection poller's
// most-recent snapshot (Claude Code versions from the npm registry +
// models from the Anthropic Models API). Behind the existing API-key
// middleware. Read-only; the snapshot is produced asynchronously by the
// runtimedetect.Poller goroutine wired in cmd/control-plane/main.go.
//
// Per kyber#375 (PR-A of kyber#374):
//   - When the cache is empty (poller hasn't run yet, or both upstreams
//     have been failing since startup), return an empty body — never 5xx.
//     The PWA renders a "detection unavailable" state on an empty list,
//     and agents are unaffected.
//   - PR-A always emits ContextWindowKnown=false + 200K floor for every
//     model. PR-D layers the operator override map on top.

package api

import (
	"errors"
	"net/http"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

// AvailableResponse is the JSON shape returned by GET /api/v1/available.
// The PWA model picker reads this directly — additive changes only;
// removing or renaming a field is a breaking change.
type AvailableResponse struct {
	ClaudeCodeVersions []string         `json:"claudeCodeVersions"`
	CodexVersions      []string         `json:"codexVersions"`
	Models             []AvailableModel `json:"models"`
	CodexModels        []AvailableModel `json:"codexModels"`
}

// AvailableModel mirrors the runtimedetect.Model fields the PWA needs.
// Kept as a separate struct so the API contract is locally explicit and
// doesn't leak internal fields (e.g., a future FetchedAt) into the public
// shape.
type AvailableModel struct {
	ID                 string `json:"id"`
	DisplayName        string `json:"displayName"`
	ContextWindow      int64  `json:"contextWindow"`
	ContextWindowKnown bool   `json:"contextWindowKnown"`
}

// handleAvailable serves GET /api/v1/available. Wired in
// registerProtectedRoutes; the API-key middleware authenticates the
// request before this runs.
func (s *Server) handleAvailable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}

	resp := AvailableResponse{
		ClaudeCodeVersions: []string{},
		CodexVersions:      []string{},
		Models:             []AvailableModel{},
		CodexModels:        []AvailableModel{},
	}

	if s.RuntimeDetectCache == nil {
		// Detection is disabled on this install. Return the empty
		// contract — PWA renders "detection unavailable".
		writeJSON(w, http.StatusOK, resp)
		return
	}

	snap, err := s.RuntimeDetectCache.Get(r.Context())
	if err != nil {
		if errors.Is(err, runtimedetect.ErrCacheEmpty) {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		// Cache backend error (Redis down): still return the empty
		// fallback so the PWA can render without an error toast on a
		// transient store hiccup. Don't echo the underlying error to
		// the client — Redis URLs / k8s details would leak.
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if snap.ClaudeCodeVersions != nil {
		resp.ClaudeCodeVersions = snap.ClaudeCodeVersions
	}
	if snap.CodexVersions != nil {
		resp.CodexVersions = snap.CodexVersions
	}
	if snap.Models != nil {
		resp.Models = make([]AvailableModel, 0, len(snap.Models))
		for _, m := range snap.Models {
			resp.Models = append(resp.Models, AvailableModel{
				ID:                 m.ID,
				DisplayName:        m.DisplayName,
				ContextWindow:      m.ContextWindow,
				ContextWindowKnown: m.ContextWindowKnown,
			})
		}
	}
	if snap.CodexModels != nil {
		resp.CodexModels = availableModels(snap.CodexModels)
	}

	writeJSON(w, http.StatusOK, resp)
}

func availableModels(models []runtimedetect.Model) []AvailableModel {
	out := make([]AvailableModel, 0, len(models))
	for _, m := range models {
		out = append(out, AvailableModel{ID: m.ID, DisplayName: m.DisplayName, ContextWindow: m.ContextWindow, ContextWindowKnown: m.ContextWindowKnown})
	}
	return out
}
