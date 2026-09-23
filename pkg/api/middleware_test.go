package api

// middleware_test.go tests unexported middleware functions that cannot be reached
// from the external api_test package.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMiddleware_RecoverPanic verifies that recoverMiddleware catches a panicking
// handler and returns 500 without propagating the panic to the caller.
func TestMiddleware_RecoverPanic(t *testing.T) {
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	wrapped := recoverMiddleware(panicHandler)

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rr := httptest.NewRecorder()

	// recoverMiddleware must not let the panic escape.
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("panic escaped recoverMiddleware: %v", rec)
		}
	}()

	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rr.Code)
	}
}

// TestLoggingMiddleware_NeverSniffsHTML verifies that a response without a
// declared Content-Type keeps net/http's sniffed type, except that markup is
// served as plain text so a browser never renders it (reflected XSS).
func TestLoggingMiddleware_NeverSniffsHTML(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	tests := []struct {
		name     string
		declared string
		body     []byte
		want     string
	}{
		{"html log line", "", []byte("<html><script>alert(1)</script>"), "text/plain; charset=utf-8"},
		{"xml or svg", "", []byte(`<?xml version="1.0"?><svg onload="alert(1)"/>`), "text/plain; charset=utf-8"},
		{"plain text keeps its sniffed type", "", []byte("pod started\n"), "text/plain; charset=utf-8"},
		{"binary keeps its sniffed type", "", png, "image/png"},
		{"declared type is untouched", "application/json", []byte(`{"a":"<b>"}`), "application/json"},
		{"declared html is the handler's choice", "text/html; charset=utf-8", []byte("<p>ok</p>"), "text/html; charset=utf-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.declared != "" {
					w.Header().Set("Content-Type", tc.declared)
				}
				_, _ = w.Write(tc.body)
			}))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
			if got := rr.Header().Get("Content-Type"); got != tc.want {
				t.Errorf("Content-Type = %q, want %q", got, tc.want)
			}
		})
	}
}
