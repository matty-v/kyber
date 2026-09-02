package api

import (
	"net/http"
	"strings"
)

// corsMiddleware returns an http.Handler middleware that adds CORS support
// for cross-origin browser clients (holocron in Phase C). Same-origin clients
// (the embedded PWA inside this binary) are unaffected — Origin header is
// either absent or matches the request URL's host.
//
// Behavior:
//   - Empty allowed list: middleware is a no-op (CORS disabled).
//   - Origin header missing: middleware is a no-op (same-origin or non-browser).
//   - Origin matches an entry in `allowed` (case-insensitive exact match):
//     echoes the origin in Access-Control-Allow-Origin (not "*", so the browser
//     can include the Authorization header) and sets Vary: Origin so caches
//     don't share responses across origins. Also opts cross-origin readers in
//     to the X-Request-ID response header via Access-Control-Expose-Headers.
//   - OPTIONS preflight from an allowed origin: returns 204 with
//     Access-Control-Allow-Methods and Access-Control-Allow-Headers populated.
//   - Any disallowed origin: no CORS headers added; browser fails the preflight
//     or rejects the cross-origin response. The handler still runs (so a
//     curl/CLI client without an Origin header still gets the real response).
func corsMiddleware(allowed []string) func(http.Handler) http.Handler {
	// Lowercase normalization: RFC 6454 origins are technically case-sensitive
	// in the host portion, but every browser normalizes to lowercase before
	// sending. Match what browsers actually do, not the strict spec.
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		allowedSet[strings.ToLower(strings.TrimSpace(o))] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			matched := false
			if origin != "" && len(allowedSet) > 0 {
				if _, ok := allowedSet[strings.ToLower(origin)]; ok {
					matched = true
				}
			}

			if matched {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				// Expose request-ID so cross-origin clients (holocron's
				// support tooling) can surface it; the server already sets
				// it on every response via requestIDMiddleware.
				w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			}

			if r.Method == http.MethodOptions {
				// Preflight: short-circuit with 204. Headers are only set if the
				// origin matched (otherwise the browser fails the preflight,
				// which is the desired behavior). 204 for unmatched OPTIONS to
				// non-existent routes is intentional — the only callers of
				// OPTIONS against this API are CORS preflights, so 204 is the
				// right default. Don't "fix" this to a 404.
				if matched {
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, Last-Event-ID")
					w.Header().Set("Access-Control-Max-Age", "600")
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
