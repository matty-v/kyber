package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// handleBrowserSession exchanges a bearer API key for a signed HttpOnly
// session cookie. It is mounted outside the normal auth wall because its job is
// to establish that session, but it authenticates the supplied bearer key
// directly and never accepts an existing cookie as the exchange credential.
func (s *Server) handleBrowserSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	if r.Header.Get("Authorization") == "" || s.auth == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "Bearer API key required")
		return
	}
	caller, err := s.auth.Authenticate(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	token, err := s.auth.CreateBrowserSession(*caller)
	if err != nil {
		// Session cookies are signed with a key derived from the shared API
		// key, so an install without one cannot issue them at all. That is a
		// configuration answer, not a server fault — say so rather than
		// returning a bare 500 the operator has to go read logs to explain.
		if errors.Is(err, errNoBrowserSessionKey) {
			writeJSONError(w, http.StatusServiceUnavailable, "session_unavailable",
				"browser sessions require KYBER_API_KEY to be set on the control plane")
			return
		}
		slog.ErrorContext(r.Context(), "creating browser session", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "session_creation_failed", "failed to create browser session")
		return
	}
	setBrowserSessionCookie(w, r, token)
	w.WriteHeader(http.StatusNoContent)
}

// setBrowserSessionCookie REPLACES any session cookie already staged on the
// response rather than appending one. A single request can legitimately reach
// here twice — authMiddleware renews a half-spent cookie before the handler
// runs, and /api/v1/rotate-api-key then issues a replacement under the new
// key. Appending would send the browser a cookie that is already dead
// alongside the live one and leave the outcome to header ordering. This is the
// only Set-Cookie the API emits, so replacing it drops nothing else.
func setBrowserSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	cookie := &http.Cookie{
		Name:     browserSessionCookie,
		Value:    token,
		Path:     "/api/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(browserSessionTTL / time.Second),
		Expires:  time.Now().Add(browserSessionTTL),
	}
	w.Header().Set("Set-Cookie", cookie.String())
}
