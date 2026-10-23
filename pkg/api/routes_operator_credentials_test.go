package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOperatorCredentialInventoryShowsActiveAndConfiguredWithoutValues(t *testing.T) {
	const adminKey, integrationKey = "inventory-sensitive-admin-value", "inventory-sensitive-integration-value"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-credentials", Namespace: "kyber-system"}, Data: map[string][]byte{
		"api-key": []byte(adminKey),
		"callers": []byte(`[{"name":"monitor","key":"inventory-sensitive-integration-value","scopes":["requests:read"],"agentResources":["kyber-system/alpha"]},{"name":"inactive","key":"inventory-sensitive-inactive-value","scopes":["metrics:read"]}]`),
	}}
	s := &api.Server{APIKey: adminKey, APIKeySecretName: secret.Name, Namespace: secret.Namespace,
		K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(secret).Build(),
		Callers: []api.ScopedCaller{
			{Name: "monitor", Key: integrationKey, Scopes: []string{"metrics:read"}},
			{Name: "removed-but-active", Key: "inventory-sensitive-removed-value"},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operator-credentials", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	res := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("inventory status/cache = %d / %q", res.Code, res.Header().Get("Cache-Control"))
	}
	for _, secret := range []string{adminKey, integrationKey, "inventory-sensitive-inactive-value", "inventory-sensitive-removed-value"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatal("inventory disclosed credential material")
		}
	}
	var body struct {
		ConfigurationAvailable bool `json:"configurationAvailable"`
		UsageAvailable         bool `json:"usageAvailable"`
		Credentials            []struct {
			Name                 string   `json:"name"`
			State                string   `json:"state"`
			AccessLevel          string   `json:"accessLevel"`
			Permissions          []string `json:"permissions"`
			Configured           bool     `json:"configured"`
			ConfigurationDiffers bool     `json:"configurationDiffers"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.ConfigurationAvailable || body.UsageAvailable || len(body.Credentials) != 4 {
		t.Fatalf("inventory coverage/size incorrect: %+v", body)
	}
	for _, c := range body.Credentials {
		switch c.Name {
		case "installation-admin":
			if c.AccessLevel != "administrator" || !c.Configured {
				t.Fatal("installation administrator not represented explicitly")
			}
		case "monitor":
			if !c.ConfigurationDiffers || !c.Configured || !strings.Contains(strings.Join(c.Permissions, ","), "fleet operational metrics") {
				t.Fatal("inventory confused active and configured grants")
			}
		case "removed-but-active":
			if c.Configured || c.State != "accepted" {
				t.Fatal("active credential removed from configuration was hidden")
			}
		case "inactive":
			if c.State != "configured-not-accepted" || len(c.Permissions) != 0 {
				t.Fatal("inactive configuration was presented as accepted authority")
			}
		default:
			t.Fatalf("unexpected credential name %q", c.Name)
		}
	}
}

func TestOperatorCredentialInventoryRestrictedBearerAndCookieDenied(t *testing.T) {
	for _, browser := range []bool{false, true} {
		s := &api.Server{APIKey: "inventory-admin-test", Namespace: "kyber-system", Callers: []api.ScopedCaller{{Name: "installation-admin", Key: "inventory-restricted-test", Scopes: []string{"lifecycle:admin"}}}}
		h := s.BuildHandler() // nil Kubernetes client: denied before backend reads
		req := httptest.NewRequest(http.MethodGet, "/api/v1/operator-credentials", nil)
		if browser {
			login := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
			login.Header.Set("Authorization", "Bearer inventory-restricted-test")
			res := httptest.NewRecorder()
			h.ServeHTTP(res, login)
			if res.Code != http.StatusNoContent || len(res.Result().Cookies()) == 0 {
				t.Fatalf("restricted session exchange failed: %d", res.Code)
			}
			req.AddCookie(res.Result().Cookies()[0])
		} else {
			req.Header.Set("Authorization", "Bearer inventory-restricted-test")
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusForbidden {
			t.Fatalf("restricted browser=%v inventory status=%d", browser, res.Code)
		}
	}
}

func TestOperatorCredentialInventoryMalformedConfigurationIsNotDisclosed(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-credentials", Namespace: "kyber-system"}, Data: map[string][]byte{"callers": []byte(`{"key":"malformed-sensitive-config-value"`)}}
	s := &api.Server{APIKey: "admin-test", APIKeySecretName: secret.Name, Namespace: secret.Namespace, K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(secret).Build()}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/operator-credentials", nil)
	r.Header.Set("Authorization", "Bearer admin-test")
	res := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(res, r)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "malformed-sensitive-config-value") || !strings.Contains(res.Body.String(), `"configurationAvailable":false`) {
		t.Fatal("malformed configuration handling disclosed values or claimed complete coverage")
	}
}

func TestOperatorCredentialInventoryReportsActualSharedKeyAuthority(t *testing.T) {
	s := &api.Server{APIKey: "root-sensitive", Callers: []api.ScopedCaller{
		{Name: "root-alias", Key: "root-sensitive"},
		{Name: "reader", Key: "shared-sensitive"},
		{Name: "writer", Key: "shared-sensitive", Scopes: []string{"lifecycle:admin"}},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operator-credentials", nil)
	req.Header.Set("Authorization", "Bearer root-sensitive")
	res := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(res, req)
	var body struct {
		Credentials []struct {
			Name            string
			State           string
			AccessLevel     string
			AuthenticatesAs string
			Permissions     []string
		}
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if res.Code != 200 || len(body.Credentials) != 4 {
		t.Fatalf("unexpected inventory status or length: %d", res.Code)
	}
	alias, reader := body.Credentials[1], body.Credentials[2]
	if alias.State != "shared-key" || alias.AccessLevel != "administrator" || alias.AuthenticatesAs != "Installation administrator" {
		t.Fatalf("root alias understated authority: %+v", alias)
	}
	if reader.State != "shared-key" || reader.AuthenticatesAs != "writer" || len(reader.Permissions) == 0 {
		t.Fatalf("shared key understated authority: %+v", reader)
	}
	for _, value := range []string{"root-sensitive", "shared-sensitive"} {
		if strings.Contains(res.Body.String(), value) {
			t.Fatal("secret exposed")
		}
	}
}

func TestOperatorCredentialInventoryComparesEffectiveSets(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-credentials", Namespace: "kyber-system"}, Data: map[string][]byte{"api-key": []byte("root-fixture"), "callers": []byte(`[{"name":"monitor","key":"monitor-fixture","scopes":["metrics:read","metrics:read"],"agentResources":["kyber-system/alpha","kyber-system/alpha"]}]`)}}
	s := &api.Server{APIKey: "root-fixture", APIKeySecretName: secret.Name, Namespace: secret.Namespace, K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(secret).Build(), Callers: []api.ScopedCaller{{Name: "monitor", Key: "monitor-fixture", Scopes: []string{"metrics:read"}, AgentResources: []string{"kyber-system/alpha"}}}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operator-credentials", nil)
	req.Header.Set("Authorization", "Bearer root-fixture")
	res := httptest.NewRecorder()
	s.BuildHandler().ServeHTTP(res, req)
	var body struct {
		Credentials []struct {
			Name                 string
			ConfigurationDiffers bool
		}
	}
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &body) != nil {
		t.Fatal("inventory unavailable")
	}
	found := false
	for _, entry := range body.Credentials {
		if entry.Name == "monitor" {
			found = true
			if entry.ConfigurationDiffers {
				t.Fatal("duplicate entries produced a false access difference")
			}
		}
	}
	if !found {
		t.Fatal("active integration missing from inventory")
	}
}
