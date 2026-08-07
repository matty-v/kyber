package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newCORSTestHandler(allowed []string) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return corsMiddleware(allowed)(inner)
}

func TestCORSMiddleware_AllowedOrigin_RegularRequest(t *testing.T) {
	h := newCORSTestHandler([]string{"https://holocron.example.com", "http://localhost:5173"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Origin", "https://holocron.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://holocron.example.com" {
		t.Errorf("Allow-Origin = %q, want https://holocron.example.com", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

func TestCORSMiddleware_AllowedOrigin_ExposesRequestID(t *testing.T) {
	h := newCORSTestHandler([]string{"https://holocron.example.com"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Origin", "https://holocron.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Expose-Headers"); got != "X-Request-ID" {
		t.Errorf("Expose-Headers = %q, want X-Request-ID", got)
	}
}

func TestCORSMiddleware_DisallowedOrigin_NoCORSHeaders(t *testing.T) {
	h := newCORSTestHandler([]string{"https://holocron.example.com"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin should be empty for disallowed origin, got %q", got)
	}
}

func TestCORSMiddleware_NoOriginHeader_NoCORSHeaders(t *testing.T) {
	h := newCORSTestHandler([]string{"https://holocron.example.com"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin should be empty when no Origin sent, got %q", got)
	}
}

func TestCORSMiddleware_PreflightAllowedOrigin(t *testing.T) {
	h := newCORSTestHandler([]string{"https://holocron.example.com"})
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/agents", nil)
	req.Header.Set("Origin", "https://holocron.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://holocron.example.com" {
		t.Errorf("Allow-Origin = %q, want https://holocron.example.com", got)
	}
	allowMethods := w.Header().Get("Access-Control-Allow-Methods")
	for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		if !contains(allowMethods, m) {
			t.Errorf("Allow-Methods missing %s: %q", m, allowMethods)
		}
	}
	allowHeaders := w.Header().Get("Access-Control-Allow-Headers")
	for _, h := range []string{"Authorization", "Content-Type"} {
		if !contains(allowHeaders, h) {
			t.Errorf("Allow-Headers missing %s: %q", h, allowHeaders)
		}
	}
}

func TestCORSMiddleware_PreflightDisallowedOrigin_NoCORSHeaders(t *testing.T) {
	h := newCORSTestHandler([]string{"https://holocron.example.com"})
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/agents", nil)
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin should be empty for disallowed origin, got %q", got)
	}
}

func TestCORSMiddleware_EmptyAllowlist_NoCORSHeaders(t *testing.T) {
	h := newCORSTestHandler(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Origin", "https://holocron.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin should be empty for nil allowlist, got %q", got)
	}
}

// Helpers

func contains(haystack, needle string) bool {
	for _, p := range splitAndTrim(haystack, ',') {
		if equalFold(p, needle) {
			return true
		}
	}
	return false
}

func splitAndTrim(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == sep {
			seg := s[start:i]
			for len(seg) > 0 && seg[0] == ' ' {
				seg = seg[1:]
			}
			for len(seg) > 0 && seg[len(seg)-1] == ' ' {
				seg = seg[:len(seg)-1]
			}
			if seg != "" {
				out = append(out, seg)
			}
			start = i + 1
		}
	}
	return out
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
