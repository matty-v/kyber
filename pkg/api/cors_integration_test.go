package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServer_BuildHandler_AppliesCORS guards against future "fixes" that move
// the corsMiddleware call from buildTopHandler to Start() only — that would
// silently make every test that calls BuildHandler() bypass CORS.
func TestServer_BuildHandler_AppliesCORS(t *testing.T) {
	s := &Server{AllowedOrigins: []string{"https://holocron.example.com"}}
	h := s.BuildHandler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://holocron.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://holocron.example.com" {
		t.Errorf("BuildHandler did not apply CORS middleware; Allow-Origin = %q, want https://holocron.example.com", got)
	}
}
