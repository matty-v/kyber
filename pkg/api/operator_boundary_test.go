package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperatorBoundaryRejectsUnclassifiedAccessBeforeDispatch(t *testing.T) {
	paths := []string{
		"/api/v1/agents", "/api/v1/agents/alpha", "/api/v1/agents/alpha/exec?mode=shell",
		"/api/v1/agents/alpha/exec?mode=attach", "/api/v1/machines/worker/exec",
		"/api/v1/agents/alpha/secrets", "/api/v1/agents/alpha/secrets/TOKEN",
		"/api/v1/agents/alpha/logs", "/api/v1/agents/alpha/logs?source=transcript",
		"/api/v1/machines/worker/logs", "/api/v1/logging/export", "/api/v1/logging/logs",
		"/api/v1/config", "/api/v1/config/export", "/api/v1/rotate-api-key",
		"/api/v1/fleet", "/api/v1/events", "/api/v1/metrics/summary", "/api/v1/github/repos",
		"/api/v1/fleet-defaults", "/api/v1/settings/anthropic-key", "/api/v1/updates",
		"/api/v1/agents/alpha/comms", "/api/v1/agents/alpha/inbound-bindings",
		"/api/v1/agents/alpha/auth", "/api/v1/agents/alpha/codex-device-auth",
		"/api/v1/agents/alpha/unknown-future-action", "/api/v1/unknown-future-route",
		"/api/v1/agents/alpha/tasks/../../exec", "/api/v1/agents/alpha/tasks//../exec",
	}
	for _, enforced := range []bool{false, true} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			for _, p := range paths {
				if method == http.MethodDelete && p == "/api/v1/agents/alpha" {
					continue // lifecycle:admin explicitly covers this exact Agent
				}
				t.Run(method+" "+p, func(t *testing.T) {
					s := &Server{Namespace: "kyber-system", AuthzEnforce: enforced}
					caller := &Caller{Name: "integration", Scopes: newScopeSet(ScopeTasksCreate, ScopeLifecycleAdmin), AgentResources: newResourceSet("kyber-system/alpha")}
					req := reqWithCaller(caller)
					req.Method = method
					req.URL = httptest.NewRequest(method, p, nil).URL
					if req.URL.Query().Get("mode") != "" {
						req.Header.Set("Connection", "Upgrade")
						req.Header.Set("Upgrade", "websocket")
					}
					called := false
					res := httptest.NewRecorder()
					s.operatorBoundary(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(res, req)
					if called || res.Code != http.StatusForbidden {
						t.Fatalf("restricted request reached dispatch: called=%v status=%d enforced=%v", called, res.Code, enforced)
					}
				})
			}
		}
	}
}

func TestOperatorBoundaryBoundsLifecycleAndMetrics(t *testing.T) {
	for _, tc := range []struct {
		method, target string
		scopes         []Scope
		allowed        bool
	}{
		{http.MethodPost, "/api/v1/agents/alpha/start", []Scope{ScopeLifecycleWrite}, true},
		{http.MethodDelete, "/api/v1/agents/alpha", []Scope{ScopeLifecycleAdmin}, true},
		{http.MethodDelete, "/api/v1/agents/beta", []Scope{ScopeLifecycleAdmin}, false},
		{http.MethodPost, "/api/v1/agents/beta/start", []Scope{ScopeLifecycleAdmin}, false},
		{http.MethodGet, "/api/v1/agents/alpha/start", []Scope{ScopeLifecycleWrite}, false},
		{http.MethodPost, "/api/v1/agents/alpha/force-needs-auth", []Scope{ScopeLifecycleWrite}, false},
		{http.MethodPost, "/api/v1/agents/alpha/force-needs-auth", []Scope{ScopeLifecycleAdmin}, true},
		{http.MethodPost, "/api/v1/agents/alpha/switch-runtime", []Scope{ScopeLifecycleAdmin}, false},
		{http.MethodGet, "/api/v1/metrics/summary", []Scope{ScopeMetricsRead}, true},
		{http.MethodPost, "/api/v1/metrics/summary", []Scope{ScopeMetricsRead}, false},
		{http.MethodGet, "/api/v1/metrics/unknown-future-route", []Scope{ScopeMetricsRead}, false},
		{http.MethodGet, "/api/v1/logging/logs", []Scope{ScopeMetricsRead}, false},
		{http.MethodGet, "/api/v1/version", nil, true},
		{http.MethodPost, "/api/v1/version", nil, false},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			s := &Server{Namespace: "kyber-system"}
			caller := &Caller{Scopes: newScopeSet(tc.scopes...), AgentResources: newResourceSet("kyber-system/alpha")}
			r := reqWithCaller(caller)
			r.Method = tc.method
			r.URL = httptest.NewRequest(tc.method, tc.target, nil).URL
			if got := s.restrictedOperatorRoute(r, caller); got != tc.allowed {
				t.Fatalf("admitted=%v, want %v", got, tc.allowed)
			}
		})
	}
}

func TestOperatorBoundaryInstalledForBearerAndBrowser(t *testing.T) {
	for _, browser := range []bool{false, true} {
		s := &Server{APIKey: "admin-test-key", Namespace: "kyber-system", Callers: []ScopedCaller{{Name: "integration", Key: "restricted-test-key"}}}
		h := s.BuildHandler() // nil backend proves rejection occurs before a read
		var cookie *http.Cookie
		if browser {
			login := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
			login.Header.Set("Authorization", "Bearer restricted-test-key")
			res := httptest.NewRecorder()
			h.ServeHTTP(res, login)
			if res.Code != http.StatusNoContent || len(res.Result().Cookies()) != 1 {
				t.Fatalf("session exchange failed: %d", res.Code)
			}
			cookie = res.Result().Cookies()[0]
		}
		for _, p := range []string{"/api/v1/agents/alpha/exec", "/api/v1/agents/alpha/secrets/TOKEN", "/api/v1/agents/alpha/logs", "/api/v1/config/export"} {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			if cookie != nil {
				r.AddCookie(cookie)
			} else {
				r.Header.Set("Authorization", "Bearer restricted-test-key")
			}
			res := httptest.NewRecorder()
			h.ServeHTTP(res, r)
			if res.Code != http.StatusForbidden {
				t.Fatalf("restricted browser=%v %s: status %d", browser, p, res.Code)
			}
		}
	}
}
