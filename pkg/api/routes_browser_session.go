package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// handleBrowserSession exchanges a bearer API key for an opaque HttpOnly
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
		slog.ErrorContext(r.Context(), "creating browser session", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "session_creation_failed", "failed to create browser session")
		return
	}
	setBrowserSessionCookie(w, r, token)
	w.WriteHeader(http.StatusNoContent)
}

func setBrowserSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     browserSessionCookie,
		Value:    token,
		Path:     "/api/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(browserSessionTTL / time.Second),
		Expires:  time.Now().Add(browserSessionTTL),
	})
}
