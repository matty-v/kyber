package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/updates"
)

// checkNowTimeout bounds a manual "Check now". Generous enough for a slow
// feed, short enough that a wedged request cannot hold a goroutine for long.
const checkNowTimeout = 30 * time.Second

// handleUpdates serves the update-checking surface:
//
//	GET  /api/v1/updates         — current version, latest available, policy,
//	                               who manages this cluster, last check result
//	PUT  /api/v1/updates/policy  — set channel / mode / pin / window
//	POST /api/v1/updates/check   — poll the release feed now, off-schedule
//	POST /api/v1/updates/apply   — install a version (creates the upgrade Job)
//
// `applySupported` in the GET response says whether apply is wired up at all,
// and `canSelfUpgrade` says whether THIS cluster may use it. The PWA needs both
// to render an honest affordance rather than discovering a 503 behind a button.
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
		//
		// Detached from the request context with its own deadline. If the
		// operator navigates away mid-poll, the request context is cancelled;
		// carrying that into the feed call aborts the poll and — worse —
		// persists "context canceled" as the cached LastError, so the card
		// reads "we stopped checking" on a healthy cluster until the next
		// hourly tick. The poll should finish and update the cache regardless
		// of whether anyone is still listening.
		pollCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), checkNowTimeout)
		defer cancel()
		writeJSON(w, http.StatusOK, s.UpdateChecker.Check(pollCtx))

	case "/api/v1/updates/apply":
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"only POST is supported on /api/v1/updates/apply")
			return
		}
		s.handleUpdateApply(w, r)

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
	//
	// Built from `next` — the policy we just validated and saved — rather than
	// re-read. Reads go through the cached client and writes do not, so a
	// re-read here can return the PRE-write policy and echo the operator's
	// change back as if it had not applied.
	writeJSON(w, http.StatusOK, s.UpdateChecker.StatusWithPolicy(r.Context(), next))
}

// handleUpdateApply starts an upgrade.
//
// Returns 202 with the run, not 200: the Job has been created, nothing has
// been installed yet. The operator polls GET /api/v1/updates and watches
// lastRun.
//
// The version is optional and defaults to the latest release the checker has
// seen. Accepting an explicit one matters for the case the default cannot
// serve: installing a specific version after a bad one, where "latest" is
// exactly what you do not want.
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if s.UpdateApplier == nil || !s.UpdateApplier.Configured() {
		writeJSONError(w, http.StatusServiceUnavailable, "UPDATE_APPLY_NOT_CONFIGURED",
			"This control plane cannot install updates. Enable selfUpgrade in the chart, "+
				"or upgrade with Helm directly.")
		return
	}

	var body struct {
		Version string `json:"version"`
	}
	// An empty body is valid — it means "the latest you know about".
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "INVALID_BODY",
				"request body is not valid JSON: "+err.Error())
			return
		}
	}

	status := s.UpdateChecker.Status(r.Context())
	version := strings.TrimSpace(body.Version)
	if version == "" {
		version = status.LatestVersion
	}
	if version == "" {
		writeJSONError(w, http.StatusBadRequest, "NO_TARGET_VERSION",
			"No version to install: this cluster has not seen a release yet. "+
				"Run a check first, or name a version explicitly.")
		return
	}

	run, err := s.UpdateApplier.Start(r.Context(), version, status.Policy)
	if err != nil {
		// A refusal is the operator's problem to fix (wrong owner, pinned,
		// already running), not a server fault — 409 rather than 500, with the
		// applier's own wording, which already explains what to do instead.
		writeJSONError(w, http.StatusConflict, "UPDATE_APPLY_REFUSED", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
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
