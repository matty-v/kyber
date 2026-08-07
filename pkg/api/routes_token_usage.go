package api

import (
	"encoding/json"
	"net/http"
)

// handleTokenUsageGet handles GET /api/v1/agents/{name}/token-usage.
//
// Returns the most-recent Snapshot pushed by the agent's in-pod reporter,
// or 404 if none is cached (TTL expired, never written, or reporter not
// running). Never blocks on slow storage.
func (s *Server) handleTokenUsageGet(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if s.TokenStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"token store not configured")
		return
	}
	snap, err := s.TokenStore.Get(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_error",
			"token store error")
		return
	}
	if snap == nil {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"no token-usage data for agent '"+name+"' — reporter may not have written yet")
		return
	}

	// #396/#500: resolve the context-window limit server-side, at serve-time.
	// The in-pod reporter sends raw used + model only (limit/pct=0 sentinel);
	// we overwrite them here so a correction (operator ConfigMap edit OR a new
	// auto-detected window) takes effect for already-running agents on their
	// next read, with no pod restart. resolveContextWindow uses the same
	// override→snapshot→LimitFor→floor precedence as /available and the pod
	// adapter (kyber#500), so the gauge stops flooring auto-detected models
	// (the #488 >100%/"unverified" symptom); it preserves the "[1m]" strip +
	// alias normalization and is nil-safe (floors to 200K, known=false).
	// Mutate a COPY, not the stored pointer: TokenStore.Get (MemoryStore)
	// returns the shared *Snapshot, so resolving in place would race a
	// concurrent reader (e.g. the agents-list serve site, which now also
	// resolves). The shallow copy is safe — every mutated field is a value.
	resolved := *snap
	limit, known := s.resolveContextWindow(r.Context(), resolved.Model)
	// Codex reports the authoritative per-turn context window in its rollout
	// JSONL. Use it when the server has no configured/detected value.
	if !known && resolved.ContextWindowKnown && resolved.Tokens.Limit > 0 {
		limit, known = resolved.Tokens.Limit, true
	}
	resolved.Tokens.Limit = limit
	resolved.ContextWindowKnown = known
	if limit > 0 {
		resolved.Percentage = 100.0 * float64(resolved.Tokens.Used) / float64(limit)
	} else {
		resolved.Percentage = 0
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=10")
	_ = json.NewEncoder(w).Encode(&resolved)
}
