package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/matty-v/kyber/pkg/updates"
)

// handleUpdates serves the update-checking surface:
//
//	GET  /api/v1/updates         — current version, latest available, policy,
//	                               who manages this cluster, last check result
//	PUT  /api/v1/updates/policy  — set channel / mode / pin / window
//	POST /api/v1/updates/check   — poll the release feed now, off-schedule
//
// There is deliberately NO apply endpoint in this build. The checker reports;
// it never mutates the cluster. `applySupported: false` in the GET response is
// the explicit signal for that, so the PWA renders an honest affordance rather
// than discovering a 404 when someone presses a button.
func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if s.UpdateChecker == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "UPDATES_NOT_CONFIGURED",
			"Update checking is not enabled on this control plane.")
		return
	}

	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/api/v1/updates":
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"only GET is supported on /api/v1/updates")
			return
		}
		writeJSON(w, http.StatusOK, s.UpdateChecker.Status(r.Context()))

	case "/api/v1/updates/policy":
		if r.Method != http.MethodPut {
			writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"only PUT is supported on /api/v1/updates/policy")
			return
		}
		s.handleUpdatePolicyPut(w, r)

	case "/api/v1/updates/check":
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"only POST is supported on /api/v1/updates/check")
			return
		}
		// Synchronous on purpose: an operator who presses "Check now" wants
		// the answer, not an acknowledgement they have to poll behind.
		writeJSON(w, http.StatusOK, s.UpdateChecker.Check(r.Context()))

	default:
		writeJSONError(w, http.StatusNotFound, "NOT_FOUND", "unknown updates route")
	}
}

func (s *Server) handleUpdatePolicyPut(w http.ResponseWriter, r *http.Request) {
	if s.UpdateStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "UPDATES_NOT_CONFIGURED",
			"Update policy storage is not configured on this control plane.")
		return
	}

	// Read the current policy first and overlay only the fields present in the
	// body. A PUT that omits `window` must not silently erase it — the PWA
	// sends one card's worth of fields, not the whole document.
	current, err := s.UpdateStore.Load(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "UPDATE_POLICY_READ_ERROR", err.Error())
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY",
			"request body is not valid JSON: "+err.Error())
		return
	}

	next := current
	if err := overlayString(raw, "channel", func(v string) { next.Channel = updates.Channel(v) }); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if err := overlayString(raw, "mode", func(v string) { next.Mode = updates.Mode(v) }); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if err := overlayString(raw, "pinnedVersion", func(v string) { next.PinnedVersion = v }); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if err := overlayString(raw, "window", func(v string) { next.Window = v }); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if err := overlayString(raw, "timeZone", func(v string) { next.TimeZone = v }); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}

	// Validation carries the operator-facing explanation for anything this
	// build refuses (mode=auto, channel=main). Pass its message straight
	// through — it says what is unsupported AND what to use instead.
	if err := next.Validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_UPDATE_POLICY", err.Error())
		return
	}

	if err := s.UpdateStore.Save(r.Context(), next); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "UPDATE_POLICY_WRITE_ERROR", err.Error())
		return
	}

	// Return the full status, not just the policy: changing a pin or a mode
	// changes what the card should say, and a second round trip to find that
	// out is a round trip the PWA should not have to make.
	writeJSON(w, http.StatusOK, s.UpdateChecker.Status(r.Context()))
}

// overlayString applies a JSON string field when present. An explicit null is
// treated as "clear it" — that is how the PWA removes a pin or a window.
func overlayString(raw map[string]json.RawMessage, field string, set func(string)) error {
	msg, ok := raw[field]
	if !ok {
		return nil
	}
	if string(msg) == "null" {
		set("")
		return nil
	}
	var v string
	if err := json.Unmarshal(msg, &v); err != nil {
		return &fieldTypeError{field: field}
	}
	set(strings.TrimSpace(v))
	return nil
}

type fieldTypeError struct{ field string }

func (e *fieldTypeError) Error() string {
	return "field " + e.field + " must be a string or null"
}
