package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

func currentCredentialRequest(handler http.Handler, key, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func readCurrentCredential(t *testing.T, w *httptest.ResponseRecorder) operatorCredentialCurrentView {
	t.Helper()
	if w.Code != 200 {
		t.Fatal("current credential read failed", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("current metadata cacheable")
	}
	var value operatorCredentialCurrentView
	if json.Unmarshal(w.Body.Bytes(), &value) != nil {
		t.Fatal("invalid current metadata")
	}
	return value
}
func TestCurrentOperatorCredentialRootAndRestrictedMetadata(t *testing.T) {
	s := &Server{Namespace: "kyber-system", APIKey: "root-test", OperatorCredentialPodUID: "11111111-1111-4111-8111-111111111111", Callers: []ScopedCaller{{Key: "key-test", Name: "Integration", PrincipalID: "principal-client", TenantID: "tenant-client", CredentialID: "credential-client", CredentialGeneration: math.MaxUint64, Scopes: []string{"metrics:read", "tasks:read"}, AgentResources: []string{"kyber-system/alpha"}, MachineResources: []string{"kyber-system/host"}}}}
	handler := s.BuildHandler()
	root := readCurrentCredential(t, currentCredentialRequest(handler, "root-test", "GET", "/api/v1/operator-credentials/current"))
	if root.Name != "installation-admin" || root.CredentialID != installationAdminCredentialID || root.AccessLevel != "administrator" || root.Generation != "1" || !root.IdentityComplete || root.ControlPlanePodUID != s.OperatorCredentialPodUID || root.Authority != "configuration" || len(root.AgentResources) != 1 || root.AgentResources[0] != "*" {
		t.Fatal("installation identity changed", root)
	}
	response := currentCredentialRequest(handler, "key-test", "GET", "/api/v1/operator-credentials/current?credentialId="+installationAdminCredentialID)
	client := readCurrentCredential(t, response)
	if client.CredentialID != "credential-client" || client.PrincipalID != "principal-client" || client.TenantID != "tenant-client" || client.AccessLevel != "restricted" || client.Generation != fmt.Sprintf("%d", uint64(math.MaxUint64)) || !client.IdentityComplete || strings.Join(client.Scopes, ",") != "metrics:read,tasks:read" || strings.Join(client.AgentResources, ",") != "kyber-system/alpha" || strings.Join(client.MachineResources, ",") != "kyber-system/host" {
		t.Fatal("own access metadata changed", client)
	}
	for _, forbidden := range []string{"root-test", "key-test", installationAdminCredentialID, "keyFrom", "secretMaterial", "credentials"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatal("unrelated metadata or material exposed", forbidden)
		}
	}
	if s.SecurityAudit != nil {
		t.Fatal("fixture unexpectedly has a database")
	}
}
func TestCurrentOperatorCredentialPreservesUnknownIdentity(t *testing.T) {
	s := &Server{Namespace: "kyber-system", APIKey: "root-test", OperatorCredentialPodUID: "not-a-pod-uid", Callers: []ScopedCaller{{Name: "Unidentified integration", Key: "key-test", Scopes: []string{}}}}
	value := readCurrentCredential(t, currentCredentialRequest(s.BuildHandler(), "key-test", "GET", "/api/v1/operator-credentials/current"))
	if value.IdentityComplete || value.CredentialID != "" || value.PrincipalID != "" || value.TenantID != "" || value.Generation != "0" || value.ControlPlanePodUID != "" || len(value.Scopes) != 0 {
		t.Fatal("read invented identity", value)
	}
}
func TestCurrentOperatorCredentialAuthenticationPrecedenceAndMethods(t *testing.T) {
	s := &Server{Namespace: "kyber-system", APIKey: "root-test", Callers: []ScopedCaller{{Name: "first", Key: "key-test", CredentialID: "first", Scopes: []string{"tasks:read"}}, {Name: "second", Key: "key-test", CredentialID: "second", Scopes: []string{"metrics:read"}}, {Name: "root-shadow", Key: "root-test", Scopes: []string{}}}}
	handler := s.BuildHandler()
	value := readCurrentCredential(t, currentCredentialRequest(handler, "key-test", "GET", "/api/v1/operator-credentials/current"))
	if value.CredentialID != "second" || strings.Join(value.Scopes, ",") != "metrics:read" {
		t.Fatal("key was attributed to a shadowed definition")
	}
	value = readCurrentCredential(t, currentCredentialRequest(handler, "root-test", "GET", "/api/v1/operator-credentials/current"))
	if value.CredentialID != installationAdminCredentialID || value.AccessLevel != "administrator" {
		t.Fatal("root precedence changed")
	}
	for _, tc := range []struct {
		key, method string
		want        int
	}{{"", "GET", 401}, {"bad-test", "GET", 401}, {"root-test", "POST", 405}, {"key-test", "POST", 403}} {
		response := currentCredentialRequest(handler, tc.key, tc.method, "/api/v1/operator-credentials/current")
		if response.Code != tc.want || strings.Contains(response.Body.String(), `"principalId"`) {
			t.Fatal("invalid request exposed current metadata", response.Code)
		}
	}
}
func TestCurrentOperatorCredentialReauthenticatesAfterEarlierSnapshot(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	s := &Server{Namespace: "kyber-system", APIKey: "old-test", OperatorCredentialAuthority: authority}
	handler := s.BuildHandler()
	request := httptest.NewRequest("GET", "/api/v1/operator-credentials/current", nil)
	request.Header.Set("Authorization", "Bearer other-admin-fixture")
	old, err := s.auth.Authenticate(request)
	if err != nil || !old.Scopes.full {
		t.Fatal("fixture did not authenticate administrator")
	}
	request = request.WithContext(context.WithValue(request.Context(), callerCtxKey{}, old))
	authority.edit(func(doc *operatorcredentials.Document) {
		doc.Credentials[1].Role = operatorcredentials.RoleRestricted
		doc.Credentials[1].Scopes = []string{"metrics:read"}
	})
	response := httptest.NewRecorder()
	s.handleCurrentOperatorCredential(response, request)
	value := readCurrentCredential(t, response)
	if value.AccessLevel != "restricted" || strings.Join(value.Scopes, ",") != "metrics:read" || value.Authority != "registry" {
		t.Fatal("earlier grants reused", value)
	}
	for _, state := range []string{"revoked", "expired", "generation", "outage", "missing-selected", "malformed-registry"} {
		t.Run(state, func(t *testing.T) {
			authority.edit(func(doc *operatorcredentials.Document) { *doc = registryFixtureDocument(t) })
			current, err := s.auth.Authenticate(request)
			if err != nil {
				t.Fatal(err)
			}
			cookie, err := s.auth.CreateBrowserSession(*current)
			if err != nil {
				t.Fatal(err)
			}
			authority.edit(func(doc *operatorcredentials.Document) {
				switch state {
				case "revoked":
					doc.Credentials[1].State = operatorcredentials.StateRevoked
					doc.Credentials[1] = doc.Credentials[1].WithSecretMaterial(nil)
				case "expired":
					at := time.Now().Add(-time.Minute)
					doc.Credentials[1].ExpiresAt = &at
				case "generation":
					doc.Credentials[1].Generation++
				case "outage":
					authority.err = operatorcredentials.ErrUnavailable
				case "missing-selected":
					authority.err = operatorcredentials.ErrUninitialized
				case "malformed-registry":
					// Revocation must erase material. A malformed authority stays unavailable
					// rather than borrowing earlier grants or configured administrator access.
					doc.Credentials[1].State = operatorcredentials.StateRevoked
				}
			})
			cookieRequest := httptest.NewRequest("GET", "/api/v1/operator-credentials/current", nil)
			cookieRequest.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: cookie})
			cookieRequest = cookieRequest.WithContext(context.WithValue(cookieRequest.Context(), callerCtxKey{}, old))
			response := httptest.NewRecorder()
			s.handleCurrentOperatorCredential(response, cookieRequest)
			want := 401
			if state == "outage" || state == "missing-selected" || state == "malformed-registry" {
				want = 503
			}
			if response.Code != want || strings.Contains(response.Body.String(), `"principalId"`) || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("retired or unavailable metadata reused", response.Code)
			}
			if want == 503 {
				stale := currentCredentialRequest(handler, "old-test", "GET", "/api/v1/operator-credentials/current")
				if stale.Code != 503 {
					t.Fatal("selected authority failure accepted configured fallback", stale.Code)
				}
			}
			authority.mu.Lock()
			authority.err = nil
			authority.mu.Unlock()
		})
	}
}
