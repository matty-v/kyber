package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The SPA handler serves the embedded PWA. Two properties matter beyond
// "the file comes back", and both were learned from real staleness on the
// dev instance:
//
//  1. A miss under /assets/ must 404. Those are content-hashed bundles, so
//     a request for one that doesn't exist means the CLIENT is stale. The
//     old behavior — fall back to index.html with a 200 and a JavaScript
//     content type — let a stale page keep running indefinitely (and let a
//     CDN cache HTML under a .js URL).
//
//  2. index.html must not be cacheable. It names the hashed bundle to load,
//     so a cached copy pins the client to an old build and reloading cannot
//     dislodge it, because the reload is answered from cache.

func TestSPA_MissingAssetReturns404(t *testing.T) {
	s := &Server{APIKey: "k"}
	h := s.BuildHandler()

	for _, path := range []string{
		"/assets/index-DEADBEEF.js",
		"/assets/index-DEADBEEF.css",
		"/assets/nested/thing.js",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rr.Code)
			}
			// The specific regression: HTML served as if it were the asset.
			if strings.Contains(strings.ToLower(rr.Body.String()), "<!doctype html") {
				t.Error("a missing asset must not be answered with index.html")
			}
		})
	}
}

// TestSPA_ClientRoutesStillFallBack: the 404 above must be scoped to
// /assets/. Deep links are the entire reason the fallback exists — if they
// 404, refreshing any page below the root breaks the app.
func TestSPA_ClientRoutesStillFallBack(t *testing.T) {
	s := &Server{APIKey: "k"}
	h := s.BuildHandler()

	for _, path := range []string{"/agents/bob", "/machines", "/settings", "/agents/bob/shell"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (client route must fall back to index.html)", rr.Code)
			}
		})
	}
}

// TestSPA_IndexIsNotCacheable covers the entry point and the two other
// version pointers. A stale sw.js is the worst of the three: it is a
// service worker that can never be replaced, because the replacement is
// fetched through the copy being replaced.
func TestSPA_IndexIsNotCacheable(t *testing.T) {
	s := &Server{APIKey: "k"}
	h := s.BuildHandler()

	for _, path := range []string{"/", "/index.html", "/agents/bob"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			cc := rr.Header().Get("Cache-Control")
			if !strings.Contains(cc, "no-store") {
				t.Errorf("Cache-Control = %q, want it to contain no-store", cc)
			}
		})
	}
}
