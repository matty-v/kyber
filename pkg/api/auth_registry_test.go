package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

type memoryOperatorAuthority struct {
	mu       sync.Mutex
	document operatorcredentials.Document
	err      error
}

func (m *memoryOperatorAuthority) Load(context.Context) (operatorcredentials.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return operatorcredentials.Snapshot{Version: "fixture-version", Document: m.document.Clone()}, m.err
}
func (m *memoryOperatorAuthority) edit(edit func(*operatorcredentials.Document)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	edit(&m.document)
}

func registryFixtureDocument(t *testing.T) operatorcredentials.Document {
	t.Helper()
	seed, err := browserSessionSigningKey("original-installation-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return (operatorcredentials.Document{SchemaVersion: 1, Namespace: "kyber-system", Credentials: []operatorcredentials.Credential{
		(operatorcredentials.Credential{ID: installationAdminCredentialID, PrincipalID: installationAdminPrincipalID, TenantID: installationTenantID, Name: "Installation administrator", Role: "administrator", State: "active", Generation: 1, PermissionRevision: 1, Source: operatorcredentials.Source{Kind: "installation"}}).WithSecretMaterial([]byte("original-installation-fixture")).WithLegacyBrowserGeneration(1),
		(operatorcredentials.Credential{ID: "other-admin", PrincipalID: "other-principal", TenantID: installationTenantID, Name: "Other administrator", Role: "administrator", State: "active", Generation: 1, PermissionRevision: 1, Source: operatorcredentials.Source{Kind: "managed"}}).WithSecretMaterial([]byte("other-admin-fixture")),
		(operatorcredentials.Credential{ID: "https://issuer.invalid/credential/client", PrincipalID: "client@example.invalid", TenantID: "tenant_client", Name: "Task client", Role: "restricted", State: "active", Generation: 1, PermissionRevision: 1, Scopes: []string{"tasks:read", "task-events:read", "metrics:read", "logs:read"}, AgentResources: []string{"kyber-system/alpha"}, Source: operatorcredentials.Source{Kind: "managed"}}).WithSecretMaterial([]byte("client-key-fixture")).WithLegacyBrowserGeneration(1),
	}}).WithLegacyBrowserSigningKey(seed, 1)
}
func registryFixtureAuthenticator(t *testing.T, authority *memoryOperatorAuthority) *APIKeyAuthenticator {
	t.Helper()
	a := NewAPIKeyAuthenticator("configured-key-must-not-be-accepted")
	a.authority = authority
	a.namespace = "kyber-system"
	return a
}
func registryBearer(key string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}
func registryCookie(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	r.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
	return r
}
func mustRegistryCaller(t *testing.T, a *APIKeyAuthenticator, r *http.Request) *Caller {
	t.Helper()
	c, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func mustRegistryCookie(t *testing.T, a *APIKeyAuthenticator, c Caller) string {
	t.Helper()
	token, err := a.CreateBrowserSession(c)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRegistryAuthorityIsCurrentAndExclusiveAcrossReplicas(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a, b := registryFixtureAuthenticator(t, authority), registryFixtureAuthenticator(t, authority)
	for _, replica := range []*APIKeyAuthenticator{a, b} {
		if _, err := replica.Authenticate(registryBearer("configured-key-must-not-be-accepted")); err == nil {
			t.Fatal("bootstrap key accepted after adoption")
		}
		c := mustRegistryCaller(t, replica, registryBearer("client-key-fixture"))
		if c.Scopes.full || c.CredentialAuthority != "registry" || !c.Scopes.Has(ScopeMetricsRead) {
			t.Fatal("incorrect registry grants")
		}
	}
	client := mustRegistryCaller(t, a, registryBearer("client-key-fixture"))
	cookie := mustRegistryCookie(t, a, *client)
	if !strings.HasPrefix(cookie, "v2.") {
		t.Fatal("new registry session uses old root dependency")
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2].Scopes = []string{"tasks:read"}
		d.Credentials[2].PermissionRevision++
	})
	for _, replica := range []*APIKeyAuthenticator{a, b} {
		current := mustRegistryCaller(t, replica, registryCookie(cookie))
		if current.Scopes.Has(ScopeMetricsRead) || !current.Scopes.Has(ScopeTasksRead) || current.PrincipalID != client.PrincipalID || current.TenantID != client.TenantID {
			t.Fatal("grant edit changed ownership or cookie kept old grants")
		}
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2] = d.Credentials[2].WithSecretMaterial(nil)
		d.Credentials[2].State = "revoked"
		d.Credentials[2].PermissionRevision++
	})
	for _, replica := range []*APIKeyAuthenticator{a, b} {
		for _, request := range []*http.Request{registryBearer("client-key-fixture"), registryCookie(cookie)} {
			if _, err := replica.Authenticate(request); err == nil {
				t.Fatal("revoked credential accepted")
			}
		}
	}
}

func TestRegistrySessionsSurviveOtherCredentialRotation(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a := registryFixtureAuthenticator(t, authority)
	client := mustRegistryCaller(t, a, registryBearer("client-key-fixture"))
	clientV2 := mustRegistryCookie(t, a, *client)
	otherAdmin := mustRegistryCaller(t, a, registryBearer("other-admin-fixture"))
	otherCookie := mustRegistryCookie(t, a, *otherAdmin)
	binding, err := browserSessionKeyBinding("original-installation-fixture", "client-key-fixture")
	if err != nil {
		t.Fatal(err)
	}
	clientV1, err := signBrowserSession("original-installation-fixture", *client, binding, time.Now(), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	root := *installationAdminCaller()
	root.CredentialID = "credential_legacy_shared_key"
	rootV1, err := signBrowserSession("original-installation-fixture", root, "", time.Now(), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if c := mustRegistryCaller(t, a, registryCookie(rootV1)); c.CredentialID != installationAdminCredentialID {
		t.Fatal("historical root identity emitted")
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[0] = d.Credentials[0].WithSecretMaterial([]byte("rotated-installation-fixture"))
		d.Credentials[0].Generation++
	})
	for _, token := range []string{clientV1, clientV2, otherCookie} {
		mustRegistryCaller(t, a, registryCookie(token))
	}
	for _, request := range []*http.Request{registryCookie(rootV1), registryBearer("original-installation-fixture")} {
		if _, err := a.Authenticate(request); err == nil {
			t.Fatal("rotated installation credential accepted")
		}
	}
	mustRegistryCaller(t, a, registryBearer("rotated-installation-fixture"))
	// Knowing the retired installation key must not let an attacker mint a V1
	// cookie that merely claims the new generation. Pin compatibility to adoption.
	forgedRoot := *installationAdminCaller()
	forgedRoot.CredentialGeneration = 2
	forged, err := signBrowserSession("original-installation-fixture", forgedRoot, "", time.Now(), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(registryCookie(forged)); err == nil {
		t.Fatal("retired administrator key forged a current-generation cookie")
	}

	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2] = d.Credentials[2].WithSecretMaterial([]byte("rotated-client-fixture"))
		d.Credentials[2].Generation++
	})
	for _, token := range []string{clientV1, clientV2} {
		if _, err := a.Authenticate(registryCookie(token)); err == nil {
			t.Fatal("rotated client session accepted")
		}
	}
	mustRegistryCaller(t, a, registryCookie(otherCookie))
}

func TestRegistryRoleIsCurrentAndNotANameOrCookieClaim(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a := registryFixtureAuthenticator(t, authority)
	admin := mustRegistryCaller(t, a, registryBearer("other-admin-fixture"))
	token := mustRegistryCookie(t, a, *admin)
	authority.edit(func(d *operatorcredentials.Document) {
		c := &d.Credentials[1]
		c.Name = "installation-admin"
		c.Role = "restricted"
		c.Scopes = []string{"metrics:read"}
		c.PermissionRevision++
	})
	current := mustRegistryCaller(t, a, registryCookie(token))
	if current.Scopes.full || !current.Scopes.Has(ScopeMetricsRead) || current.Name != "installation-admin" {
		t.Fatal("cookie or name preserved former administrator grants")
	}
	if _, err := a.CreateBrowserSession(*admin); err != nil {
		t.Fatal("same identity cannot renew after grant change", err)
	}
}

func TestRegistryFailuresNeverRestoreBootstrap(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t), err: operatorcredentials.ErrUninitialized}
	a := registryFixtureAuthenticator(t, authority)
	mustRegistryCaller(t, a, registryBearer("configured-key-must-not-be-accepted"))
	authority.mu.Lock()
	authority.err = nil
	authority.mu.Unlock()
	mustRegistryCaller(t, a, registryBearer("client-key-fixture"))
	for _, failure := range []error{operatorcredentials.ErrUninitialized, errors.New("private transport detail"), errors.Join(operatorcredentials.ErrUninitialized, operatorcredentials.ErrUnavailable)} {
		authority.mu.Lock()
		authority.err = failure
		authority.mu.Unlock()
		for _, key := range []string{"client-key-fixture", "configured-key-must-not-be-accepted"} {
			if _, err := a.Authenticate(registryBearer(key)); err != errOperatorAuthorityUnavailable {
				t.Fatal("authority failure leaked details or restored configuration", err)
			}
		}
	}
}

func TestRegistryCompilerRejectsUnsupportedAuthority(t *testing.T) {
	for name, edit := range map[string]func(*operatorcredentials.Document){
		"new scope": func(d *operatorcredentials.Document) { d.Credentials[2].Scopes = []string{"future:grant"} },
		"wrong installation": func(d *operatorcredentials.Document) {
			d.Namespace = "other"
			d.Credentials[2].AgentResources = []string{"other/alpha"}
		},
		"duplicate material": func(d *operatorcredentials.Document) {
			d.Credentials[2] = d.Credentials[2].WithSecretMaterial(d.Credentials[1].SecretMaterial())
		},
		"unsupported schema": func(d *operatorcredentials.Document) { d.SchemaVersion = 2 },
		"malformed seed":     func(d *operatorcredentials.Document) { *d = d.WithLegacyBrowserSigningKey([]byte("bad"), 1) },
	} {
		t.Run(name, func(t *testing.T) {
			authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
			authority.edit(edit)
			a := registryFixtureAuthenticator(t, authority)
			if _, err := a.Authenticate(registryBearer("other-admin-fixture")); err != errOperatorAuthorityUnavailable {
				t.Fatal("invalid registry accepted", err)
			}
		})
	}
}

func TestRegistryHTTPUnavailableAndSafeInventory(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	s := &Server{Namespace: "kyber-system", APIKey: "configured-key-must-not-be-accepted", APIKeySecretName: "never-write-source", OperatorCredentialAuthority: authority}
	h := s.BuildHandler()
	request := registryBearer("other-admin-fixture")
	request.URL.Path = "/api/v1/operator-credentials"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "installation-registry") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("registry inventory unavailable", rec.Code)
	}
	for _, private := range []string{"other-admin-fixture", "client-key-fixture", "legacyBrowserSigningKey", "registry.json", "materialRef"} {
		if strings.Contains(rec.Body.String(), private) {
			t.Fatal("inventory leaked private material")
		}
	}
	request = registryBearer("other-admin-fixture")
	request.Method = http.MethodPost
	request.URL.Path = "/api/v1/rotate-api-key"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	if rec.Code != 409 {
		t.Fatal("old rotation path reached source Secret", rec.Code)
	}
	authority.mu.Lock()
	authority.err = errors.New("private backend detail")
	authority.mu.Unlock()
	for _, path := range []string{"/api/v1/version", "/api/v1/browser-session"} {
		request = registryBearer("other-admin-fixture")
		request.URL.Path = path
		if strings.Contains(path, "browser-session") {
			request.Method = http.MethodPost
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, request)
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), "authentication_unavailable") || strings.Contains(rec.Body.String(), "private backend detail") || rec.Header().Get("Set-Cookie") != "" {
			t.Fatal("authority outage became login failure or leaked details", rec.Code)
		}
	}
}

func TestRegistryTaskCursorsUseCredentialIdentity(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a := registryFixtureAuthenticator(t, authority)
	s := &Server{Namespace: "kyber-system", auth: a}
	admin := mustRegistryCaller(t, a, registryBearer("other-admin-fixture"))
	r := registryBearer("other-admin-fixture")
	r = r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, admin))
	cursor, err := s.encodeTaskEventCursor(r, "alpha", "task-fixture", 7)
	if err != nil {
		t.Fatal(err)
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[1].Name = "Renamed admin"
		d.Credentials[0] = d.Credentials[0].WithSecretMaterial([]byte("rotated-root"))
		d.Credentials[0].Generation++
	})
	if sequence, err := s.decodeTaskEventCursor(r, "alpha", "task-fixture", cursor); err != nil || sequence != 7 {
		t.Fatal("unrelated rotation or rename broke cursor", err)
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[1] = d.Credentials[1].WithSecretMaterial([]byte("rotated-admin"))
		d.Credentials[1].Generation++
	})
	if _, err := s.decodeTaskEventCursor(r, "alpha", "task-fixture", cursor); err == nil {
		t.Fatal("rotated credential cursor remained valid")
	}
}

func TestRegistryCookieRejectsTamperingExpiryAndForeignIdentity(t *testing.T) {
	document := registryFixtureDocument(t)
	now := time.Now()
	registry, err := compileRegistryCredentials(document, now)
	if err != nil {
		t.Fatal(err)
	}
	caller := registry.byID["other-admin"].caller
	token, err := registry.createBrowserSession(caller, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{token + "x", strings.Replace(token, "v2.", "v3.", 1), strings.Repeat("x", 4097)} {
		if _, _, err := registry.browserCaller(bad, now); err == nil {
			t.Fatal("invalid cookie accepted")
		}
	}
	if _, _, err := registry.browserCaller(token, now.Add(browserSessionTTL)); err == nil {
		t.Fatal("expired cookie accepted")
	}
	// Even a valid HMAC cannot select a different tenant or extend a token
	// beyond its bounded lifetime. Possessing one key grants no other identity.
	key, err := registrySessionKey("other-admin-fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, claims := range []registryBrowserClaims{
		{CredentialID: caller.CredentialID, PrincipalID: caller.PrincipalID, TenantID: "other-tenant", Generation: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()},
		{CredentialID: caller.CredentialID, PrincipalID: caller.PrincipalID, TenantID: caller.TenantID, Generation: 1, IssuedAt: -9223372036854775807, ExpiresAt: 9223372036854775807},
		{CredentialID: caller.CredentialID, PrincipalID: caller.PrincipalID, TenantID: caller.TenantID, Generation: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(2 * browserSessionTTL).Unix()},
		{CredentialID: installationAdminCredentialID, PrincipalID: installationAdminPrincipalID, TenantID: installationTenantID, Generation: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()},
	} {
		raw, _ := json.Marshal(claims)
		signed := "v2." + base64.RawURLEncoding.EncodeToString(raw)
		bad := signed + "." + base64.RawURLEncoding.EncodeToString(browserSessionMAC(key, signed))
		if _, _, err := registry.browserCaller(bad, now); err == nil {
			t.Fatal("signed foreign/invalid claims accepted")
		}
	}

	foreign := caller
	foreign.TenantID = "other-tenant"
	if _, err := registry.createBrowserSession(foreign, now); err == nil {
		t.Fatal("foreign tenant session minted")
	}
}

func TestRegistryExpiryAndHistoricalAdministratorDemotion(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a := registryFixtureAuthenticator(t, authority)
	client := mustRegistryCaller(t, a, registryBearer("client-key-fixture"))
	clientCookie := mustRegistryCookie(t, a, *client)
	oldRoot, err := signBrowserSession("original-installation-fixture", *installationAdminCaller(), "", time.Now(), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	authority.edit(func(d *operatorcredentials.Document) {
		expired := time.Now().Add(-time.Second)
		d.Credentials[2].ExpiresAt = &expired
		d.Credentials[2].PermissionRevision++
		root := &d.Credentials[0]
		root.Role = "restricted"
		root.Scopes = []string{"task-events:read"}
		root.AgentResources = []string{"kyber-system/alpha"}
		root.PermissionRevision++
	})
	for _, request := range []*http.Request{registryBearer("client-key-fixture"), registryCookie(clientCookie)} {
		if _, err := a.Authenticate(request); err == nil {
			t.Fatal("expired key or session accepted")
		}
	}
	root := mustRegistryCaller(t, a, registryCookie(oldRoot))
	if root.Scopes.full || !root.Scopes.Has(ScopeTaskEventsRead) {
		t.Fatal("historical full-scope bit retained administrator access")
	}
	if !taskCursorCredentialMatches("credential_legacy_shared_key", root) {
		t.Fatal("historical installation cursor lost identity after grant narrowing")
	}
	impostor := *root
	impostor.PrincipalID = "other-principal"
	if taskCursorCredentialMatches("credential_legacy_shared_key", &impostor) {
		t.Fatal("foreign principal borrowed historical installation identity")
	}
}

func TestRegistryRejectsUnusableHTTPMaterial(t *testing.T) {
	for _, key := range []string{"line\nbreak", "nul\x00byte", "trailing ", "trailing\t", "delete\x7fbyte"} {
		authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
		authority.edit(func(d *operatorcredentials.Document) {
			d.Credentials[2] = d.Credentials[2].WithSecretMaterial([]byte(key))
		})
		if _, err := registryFixtureAuthenticator(t, authority).Authenticate(registryBearer("other-admin-fixture")); err != errOperatorAuthorityUnavailable {
			t.Fatal("unusable credential accepted for activation", err)
		}
	}
}

func TestRegistryPreIdentityCookieSurvivesRename(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a := registryFixtureAuthenticator(t, authority)
	binding, err := browserSessionKeyBinding("original-installation-fixture", "client-key-fixture")
	if err != nil {
		t.Fatal(err)
	}
	token, err := signBrowserSession("original-installation-fixture", Caller{Name: "Earlier display name", Scopes: newScopeSet(ScopeMetricsRead)}, binding, time.Now(), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	current := mustRegistryCaller(t, a, registryCookie(token))
	authority.edit(func(d *operatorcredentials.Document) { d.Credentials[2].Name = "Renamed integration" })
	renamed := mustRegistryCaller(t, a, registryCookie(token))
	if renamed.Name != "Renamed integration" || !sameCredentialIdentity(*current, *renamed) {
		t.Fatal("rename changed identity or invalidated pre-identity cookie")
	}
}

func TestRegistryPreIdentityCookieCannotRebindAfterRotation(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a := registryFixtureAuthenticator(t, authority)
	binding, err := browserSessionKeyBinding("original-installation-fixture", "client-key-fixture")
	if err != nil {
		t.Fatal(err)
	}
	token, err := signBrowserSession("original-installation-fixture", Caller{Name: "Old client", Scopes: newScopeSet(ScopeMetricsRead)}, binding, time.Now(), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	mustRegistryCaller(t, a, registryCookie(token))
	authority.edit(func(d *operatorcredentials.Document) { d.Credentials[2].Generation += 2 })
	if _, err := a.Authenticate(registryCookie(token)); err == nil {
		t.Fatal("pre-identity cookie revived after key material reuse")
	}
	// New credentials have no V1 adoption binding, even if an administrator
	// deliberately reuses material that once signed an older client's session.
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2].ID = "new-client"
		d.Credentials[2] = d.Credentials[2].WithLegacyBrowserGeneration(0)
	})
	if _, err := a.Authenticate(registryCookie(token)); err == nil {
		t.Fatal("pre-identity cookie rebound to a newly issued credential")
	}
}

func TestRegistryCompiledDiagnosticsAreRedacted(t *testing.T) {
	compiled, err := compileRegistryCredentials(registryFixtureDocument(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{compiled, *compiled} {
		for _, format := range []string{"%v", "%+v", "%#v", "%q", "%d", "%x"} {
			diagnostic := fmt.Sprintf(format, value)
			if !strings.Contains(diagnostic, "[redacted]") || strings.Contains(diagnostic, "fixture") || strings.Contains(diagnostic, "compatibilityKey") {
				t.Fatal("compiled authority exposed private diagnostics")
			}
		}
	}
}
