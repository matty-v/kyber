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
