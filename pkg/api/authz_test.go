package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestRequiredScopeForPhase(t *testing.T) {
	cases := []struct {
		phase kyberv1.AgentPhase
		want  Scope
	}{
		{kyberv1.AgentPhaseRunning, ScopeLifecycleWrite},    // start / oauth resume
		{kyberv1.AgentPhaseStopped, ScopeLifecycleWrite},    // stop (fail-safe)
		{kyberv1.AgentPhaseRestarting, ScopeLifecycleWrite}, // restart
		{kyberv1.AgentPhaseNeedsAuth, ScopeLifecycleAdmin},  // force-needs-auth (impactful)
	}
	for _, c := range cases {
		if got := requiredScopeForPhase(c.phase); got != c.want {
			t.Errorf("requiredScopeForPhase(%s) = %s, want %s", c.phase, got, c.want)
		}
	}
}

// reqWithCaller builds a request carrying the given caller in context, as
// authMiddleware would have stashed it.
func reqWithCaller(c *Caller) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/force-needs-auth", nil)
	if c != nil {
		r = r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, c))
	}
	return r
}

func TestAuthorizePhase_Enforcing(t *testing.T) {
	s := &Server{AuthzEnforce: true}
	writeCaller := &Caller{Name: "pwa", Scopes: newScopeSet(ScopeLifecycleWrite)}
	adminCaller := &Caller{Name: "ops", Scopes: newScopeSet(ScopeLifecycleAdmin)}
	legacyCaller := &Caller{Name: "legacy", Scopes: newFullScopeSet()}

	cases := []struct {
		name      string
		caller    *Caller
		phase     kyberv1.AgentPhase
		wantAllow bool
		wantCode  int
	}{
		{"write key on stop → allow", writeCaller, kyberv1.AgentPhaseStopped, true, 0},
		{"write key on restart → allow", writeCaller, kyberv1.AgentPhaseRestarting, true, 0},
		{"write key on force-needs-auth → 403", writeCaller, kyberv1.AgentPhaseNeedsAuth, false, http.StatusForbidden},
		{"admin key on force-needs-auth → allow", adminCaller, kyberv1.AgentPhaseNeedsAuth, true, 0},
		{"legacy full-scope on force-needs-auth → allow", legacyCaller, kyberv1.AgentPhaseNeedsAuth, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			got := s.authorizePhase(w, reqWithCaller(c.caller), "x", c.phase)
			if got != c.wantAllow {
				t.Errorf("authorizePhase allow = %v, want %v", got, c.wantAllow)
			}
			if !c.wantAllow && w.Code != c.wantCode {
				t.Errorf("deny HTTP code = %d, want %d", w.Code, c.wantCode)
			}
			if !c.wantAllow {
				// Non-leaky: must not name the required scope or granted scopes.
				body := w.Body.String()
				if strings.Contains(body, "lifecycle:") {
					t.Errorf("403 body leaks scope detail: %q", body)
				}
			}
		})
	}
}

// TestAuthorizeAction covers the shared caller-gate helper that authorizePhase
// delegates to and that the destructive DELETE paths (delete / machine-delete)
// call directly with ScopeLifecycleAdmin (kyber#565).
func TestAuthorizeAction_Enforcing(t *testing.T) {
	s := &Server{AuthzEnforce: true}
	writeCaller := &Caller{Name: "pwa", Scopes: newScopeSet(ScopeLifecycleWrite)}
	adminCaller := &Caller{Name: "ops", Scopes: newScopeSet(ScopeLifecycleAdmin)}
	legacyCaller := &Caller{Name: "legacy", Scopes: newFullScopeSet()}

	cases := []struct {
		name      string
		caller    *Caller
		action    string
		required  Scope
		wantAllow bool
		wantCode  int
	}{
		{"write key on delete (admin) → 403", writeCaller, "delete", ScopeLifecycleAdmin, false, http.StatusForbidden},
		{"admin key on delete → allow", adminCaller, "delete", ScopeLifecycleAdmin, true, 0},
		{"legacy full-scope on delete → allow", legacyCaller, "delete", ScopeLifecycleAdmin, true, 0},
		{"admin key on machine-delete → allow", adminCaller, "machine-delete", ScopeLifecycleAdmin, true, 0},
		{"write key on machine-delete (admin) → 403", writeCaller, "machine-delete", ScopeLifecycleAdmin, false, http.StatusForbidden},
		{"write key on repair-runtime → allow", writeCaller, "repair-runtime", ScopeLifecycleWrite, true, 0},
		{"nil caller on repair-runtime → 403", nil, "repair-runtime", ScopeLifecycleWrite, false, http.StatusForbidden},
		{"nil caller on delete → 403", nil, "delete", ScopeLifecycleAdmin, false, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			got := s.authorizeAction(w, reqWithCaller(c.caller), "x", c.action, c.required)
			if got != c.wantAllow {
				t.Errorf("authorizeAction allow = %v, want %v", got, c.wantAllow)
			}
			if !c.wantAllow && w.Code != c.wantCode {
				t.Errorf("deny HTTP code = %d, want %d", w.Code, c.wantCode)
			}
			if !c.wantAllow {
				// Non-leaky: must not name the required scope or granted scopes.
				if strings.Contains(w.Body.String(), "lifecycle:") {
					t.Errorf("403 body leaks scope detail: %q", w.Body.String())
				}
			}
		})
	}
}

func TestAuthorizeAction_PermissiveModeNeverBlocks(t *testing.T) {
	// Default (enforce off): even an under-scoped caller is allowed through on the
	// strictly-highest-scope DELETE — would-deny audit-logged, request proceeds.
	s := &Server{AuthzEnforce: false}
	writeCaller := &Caller{Name: "pwa", Scopes: newScopeSet(ScopeLifecycleWrite)}

	w := httptest.NewRecorder()
	if !s.authorizeAction(w, reqWithCaller(writeCaller), "x", "delete", ScopeLifecycleAdmin) {
		t.Error("permissive mode must allow an under-scoped caller through")
	}
	if w.Code != http.StatusOK {
		t.Errorf("permissive mode must not write an error response, got code %d", w.Code)
	}
}

func TestAuthorizePhase_PermissiveModeNeverBlocks(t *testing.T) {
	// Default (enforce off): even an under-scoped caller is allowed through —
	// the decision is audit-logged as would-deny but the request proceeds, so
	// single-key installs and the migration window are unaffected.
	s := &Server{AuthzEnforce: false}
	writeCaller := &Caller{Name: "pwa", Scopes: newScopeSet(ScopeLifecycleWrite)}

	w := httptest.NewRecorder()
	if !s.authorizePhase(w, reqWithCaller(writeCaller), "x", kyberv1.AgentPhaseNeedsAuth) {
		t.Error("permissive mode must allow an under-scoped caller through")
	}
	if w.Code != http.StatusOK { // recorder defaults to 200; nothing written
		t.Errorf("permissive mode must not write an error response, got code %d", w.Code)
	}
}

func TestRequireScopeAlwaysEnforces(t *testing.T) {
	s := &Server{AuthzEnforce: false}
	writeCaller := &Caller{Name: "gateway", Scopes: newScopeSet(ScopeRequestsWrite)}
	lifecycleCaller := &Caller{Name: "pwa", Scopes: newScopeSet(ScopeLifecycleWrite)}

	allowed := httptest.NewRecorder()
	if !s.requireScope(allowed, reqWithCaller(writeCaller), "x", "request-submit", ScopeRequestsWrite) {
		t.Error("matching request scope must be allowed")
	}

	denied := httptest.NewRecorder()
	if s.requireScope(denied, reqWithCaller(lifecycleCaller), "x", "request-submit", ScopeRequestsWrite) {
		t.Error("lifecycle scope must not inherit request access")
	}
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied code = %d, want 403", denied.Code)
	}
	if strings.Contains(denied.Body.String(), "requests:") {
		t.Errorf("403 body leaks scope detail: %q", denied.Body.String())
	}
}
