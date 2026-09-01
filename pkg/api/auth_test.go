package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWithBearer(key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/stop", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}

func TestAPIKeyAuthenticator_LegacyKeyIsFullScope(t *testing.T) {
	a := NewAPIKeyAuthenticator("legacy-key")
	caller, err := a.Authenticate(reqWithBearer("legacy-key"))
	if err != nil {
		t.Fatalf("legacy key should authenticate: %v", err)
	}
	if caller.Name != "legacy" {
		t.Errorf("caller name = %q, want legacy", caller.Name)
	}
	// Full scope satisfies every check, including the highest verb.
	if !caller.Scopes.Has(ScopeLifecycleAdmin) || !caller.Scopes.Has(ScopeLifecycleWrite) {
		t.Error("legacy key must resolve to a full-scope caller")
	}
}

func TestAPIKeyAuthenticator_ScopedKeyResolvesToItsScopes(t *testing.T) {
	a := NewAPIKeyAuthenticator("legacy-key",
		ScopedCaller{Name: "pwa", Key: "write-key", Scopes: []string{"lifecycle:write"}},
		ScopedCaller{Name: "ops", Key: "admin-key", Scopes: []string{"lifecycle:admin"}},
	)

	pwa, err := a.Authenticate(reqWithBearer("write-key"))
	if err != nil {
		t.Fatalf("scoped write key should authenticate: %v", err)
	}
	if pwa.Name != "pwa" {
		t.Errorf("caller name = %q, want pwa", pwa.Name)
	}
	if !pwa.Scopes.Has(ScopeLifecycleWrite) {
		t.Error("pwa must have lifecycle:write")
	}
	if pwa.Scopes.Has(ScopeLifecycleAdmin) {
		t.Error("pwa (write-only) must NOT have lifecycle:admin — privilege ordering")
	}

	ops, err := a.Authenticate(reqWithBearer("admin-key"))
	if err != nil {
		t.Fatalf("scoped admin key should authenticate: %v", err)
	}
	// admin ⊃ write.
	if !ops.Scopes.Has(ScopeLifecycleAdmin) || !ops.Scopes.Has(ScopeLifecycleWrite) {
		t.Error("ops (admin) must satisfy both admin and write")
	}
}

func TestAPIKeyAuthenticator_InvalidAndMissingKey(t *testing.T) {
	a := NewAPIKeyAuthenticator("legacy-key",
		ScopedCaller{Name: "pwa", Key: "write-key", Scopes: []string{"lifecycle:write"}})

	if _, err := a.Authenticate(reqWithBearer("wrong-key")); err == nil {
		t.Error("an unknown key must be rejected (401)")
	}
	if _, err := a.Authenticate(reqWithBearer("")); err == nil {
		t.Error("a missing Authorization header must be rejected (401)")
	}
}

func TestAPIKeyAuthenticator_TokenQueryParam(t *testing.T) {
	a := NewAPIKeyAuthenticator("legacy-key")

	// On a WebSocket upgrade request, ?token= authenticates (browsers cannot
	// set custom headers during the upgrade handshake).
	ws := httptest.NewRequest(http.MethodGet, "/api/v1/agents/x/logs?token=legacy-key", nil)
	ws.Header.Set("Connection", "Upgrade")
	ws.Header.Set("Upgrade", "websocket")
	caller, err := a.Authenticate(ws)
	if err != nil {
		t.Fatalf("?token= should authenticate on a WebSocket upgrade: %v", err)
	}
	if caller.Name != "legacy" {
		t.Errorf("caller name = %q, want legacy", caller.Name)
	}

	// On a plain REST request, ?token= must be rejected: keys in URLs leak
	// into proxy/ingress access logs (kyber risk audit H2).
	rest := httptest.NewRequest(http.MethodGet, "/api/v1/agents/x/logs?token=legacy-key", nil)
	if _, err := a.Authenticate(rest); err == nil {
		t.Error("?token= on a non-upgrade request must be rejected (401)")
	}
}

func TestAPIKeyAuthenticator_RotationKeepsScopedCallers(t *testing.T) {
	a := NewAPIKeyAuthenticator("old-legacy",
		ScopedCaller{Name: "pwa", Key: "write-key", Scopes: []string{"lifecycle:write"}})
	a.SetKey("new-legacy")

	if _, err := a.Authenticate(reqWithBearer("old-legacy")); err == nil {
		t.Error("old legacy key must be rejected after rotation")
	}
	if _, err := a.Authenticate(reqWithBearer("new-legacy")); err != nil {
		t.Errorf("new legacy key must authenticate after rotation: %v", err)
	}
	// Scoped callers survive a legacy rotation.
	if _, err := a.Authenticate(reqWithBearer("write-key")); err != nil {
		t.Errorf("scoped key must still authenticate after legacy rotation: %v", err)
	}
}

func TestParseScopedCallers(t *testing.T) {
	good := `[{"name":"pwa","key":"k1","scopes":["lifecycle:write"]},{"name":"ops","key":"k2","scopes":["lifecycle:admin"]},{"name":"gateway","key":"k3","scopes":["requests:write","requests:read"]}]`
	cs, err := ParseScopedCallers(good)
	if err != nil {
		t.Fatalf("valid callers JSON should parse: %v", err)
	}
	if len(cs) != 3 {
		t.Fatalf("expected 3 callers, got %d", len(cs))
	}

	if _, err := ParseScopedCallers(""); err != nil {
		t.Errorf("empty config must be a no-op, got error: %v", err)
	}
	// Fail-closed on bad config rather than silently granting.
	if _, err := ParseScopedCallers(`[{"name":"x","key":"k","scopes":["lifecycle:superuser"]}]`); err == nil {
		t.Error("an unknown scope must be rejected")
	}
	if _, err := ParseScopedCallers(`[{"name":"x","scopes":["lifecycle:write"]}]`); err == nil {
		t.Error("a caller missing its key must be rejected")
	}
	if _, err := ParseScopedCallers(`not json`); err == nil {
		t.Error("malformed JSON must be rejected")
	}
}

func TestScopeSet_AdminImpliesWrite(t *testing.T) {
	admin := newScopeSet(ScopeLifecycleAdmin)
	if !admin.Has(ScopeLifecycleWrite) {
		t.Error("admin must imply write (admin ⊃ write)")
	}
	write := newScopeSet(ScopeLifecycleWrite)
	if write.Has(ScopeLifecycleAdmin) {
		t.Error("write must NOT imply admin")
	}
	if !newFullScopeSet().Has(ScopeLifecycleAdmin) {
		t.Error("full scope must satisfy admin")
	}
}

func TestScopeSet_RequestScopesAreIndependent(t *testing.T) {
	write := newScopeSet(ScopeRequestsWrite)
	if !write.Has(ScopeRequestsWrite) || write.Has(ScopeRequestsRead) || write.Has(ScopeLifecycleWrite) {
		t.Error("requests:write must grant only request submission")
	}
	read := newScopeSet(ScopeRequestsRead)
	if !read.Has(ScopeRequestsRead) || read.Has(ScopeRequestsWrite) || read.Has(ScopeLifecycleWrite) {
		t.Error("requests:read must grant only request reads")
	}
}

func TestScopeSet_TaskScopesAreIndependent(t *testing.T) {
	all := []Scope{ScopeTasksCreate, ScopeTasksRead, ScopeTasksList, ScopeTasksContinue, ScopeTasksCancel, ScopeTaskResultsRead, ScopeTaskEventsRead, ScopeTasksAdmin, ScopeTasksPlatformAdmin}
	for _, granted := range all {
		set := newScopeSet(granted)
		for _, checked := range all {
			if got := set.Has(checked); got != (checked == granted) {
				t.Errorf("granting %q, Has(%q)=%v; task scopes must not imply one another", granted, checked, got)
			}
		}
	}
}

func TestParseScopedCallers_TaskScopesRequireStableSecurityEnvelope(t *testing.T) {
	good := `[{"name":"gateway","principalId":"principal_gateway","tenantId":"tenant_acme","credentialId":"credential_gateway","credentialGeneration":2,"agentResources":["kyber-system/kiosk"],"key":"secret","scopes":["tasks:create","tasks:read"]}]`
	callers, err := ParseScopedCallers(good)
	if err != nil {
		t.Fatalf("task caller with stable envelope should parse: %v", err)
	}
	if callers[0].PrincipalID != "principal_gateway" || callers[0].CredentialGeneration != 2 {
		t.Fatalf("security envelope not preserved: %+v", callers[0])
	}

	for name, doc := range map[string]string{
		"principal":  `[{"name":"gateway","tenantId":"tenant_acme","credentialId":"credential_gateway","credentialGeneration":1,"agentResources":["kyber-system/kiosk"],"key":"secret","scopes":["tasks:read"]}]`,
		"tenant":     `[{"name":"gateway","principalId":"principal_gateway","credentialId":"credential_gateway","credentialGeneration":1,"agentResources":["kyber-system/kiosk"],"key":"secret","scopes":["tasks:read"]}]`,
		"credential": `[{"name":"gateway","principalId":"principal_gateway","tenantId":"tenant_acme","credentialGeneration":1,"agentResources":["kyber-system/kiosk"],"key":"secret","scopes":["tasks:read"]}]`,
		"generation": `[{"name":"gateway","principalId":"principal_gateway","tenantId":"tenant_acme","credentialId":"credential_gateway","agentResources":["kyber-system/kiosk"],"key":"secret","scopes":["tasks:read"]}]`,
		"resources":  `[{"name":"gateway","principalId":"principal_gateway","tenantId":"tenant_acme","credentialId":"credential_gateway","credentialGeneration":1,"key":"secret","scopes":["tasks:read"]}]`,
	} {
		if _, err := ParseScopedCallers(doc); err == nil {
			t.Errorf("task caller missing %s must be rejected", name)
		}
	}
	for _, resource := range []string{"*", "kiosk", "/kiosk", "kyber-system/", "Kyber-System/kiosk", "kyber-system/kiosk/extra"} {
		doc := `[{"name":"gateway","principalId":"principal_gateway","tenantId":"tenant_acme","credentialId":"credential_gateway","credentialGeneration":1,"agentResources":["` + resource + `"],"key":"secret","scopes":["tasks:read"]}]`
		if _, err := ParseScopedCallers(doc); err == nil {
			t.Errorf("malformed or wildcard task resource %q must be rejected", resource)
		}
	}
}

func TestParseScopedCallers_KeyFrom(t *testing.T) {
	// keyFrom is the kyber#557 alternative to inline key: a Secret reference,
	// resolved at startup. Exactly one of key/keyFrom must be set — both or
	// neither is a parse error (fail-closed, the existing contract).
	good := `[{"name":"some-caller","keyFrom":{"secret":"some-caller-api-key","key":"api-key"},"scopes":["lifecycle:write"]}]`
	cs, err := ParseScopedCallers(good)
	if err != nil {
		t.Fatalf("valid keyFrom entry should parse: %v", err)
	}
	if cs[0].KeyFrom == nil || cs[0].KeyFrom.Secret != "some-caller-api-key" || cs[0].KeyFrom.Key != "api-key" {
		t.Errorf("keyFrom not parsed: %+v", cs[0].KeyFrom)
	}
	if cs[0].Key != "" {
		t.Errorf("a keyFrom entry must carry no inline key value, got %q", cs[0].Key)
	}

	// A mixed doc (inline + keyFrom entries) is valid.
	mixed := `[{"name":"inline","key":"k1","scopes":["lifecycle:write"]},{"name":"ref","keyFrom":{"secret":"s","key":"k"},"scopes":["lifecycle:write"]}]`
	if _, err := ParseScopedCallers(mixed); err != nil {
		t.Errorf("mixed inline+keyFrom doc should parse: %v", err)
	}

	for name, doc := range map[string]string{
		"both key and keyFrom":     `[{"name":"x","key":"k","keyFrom":{"secret":"s","key":"d"},"scopes":["lifecycle:write"]}]`,
		"neither key nor keyFrom":  `[{"name":"x","scopes":["lifecycle:write"]}]`,
		"keyFrom missing secret":   `[{"name":"x","keyFrom":{"key":"d"},"scopes":["lifecycle:write"]}]`,
		"keyFrom missing data key": `[{"name":"x","keyFrom":{"secret":"s"},"scopes":["lifecycle:write"]}]`,
	} {
		if _, err := ParseScopedCallers(doc); err == nil {
			t.Errorf("%s must be a parse error (fail-closed)", name)
		}
	}

	// Unknown scopes stay rejected on keyFrom entries too.
	if _, err := ParseScopedCallers(`[{"name":"x","keyFrom":{"secret":"s","key":"d"},"scopes":["lifecycle:superuser"]}]`); err == nil {
		t.Error("unknown scope on a keyFrom entry must be rejected")
	}
}
