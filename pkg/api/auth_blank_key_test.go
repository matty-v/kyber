package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestBlankConfiguredKeysNeverGrantAdministratorAccess(t *testing.T) {
	for _, key := range []string{"", " ", "\n", "\t"} {
		s := &Server{APIKey: key, Namespace: "kyber-system"}
		server := httptest.NewServer(s.BuildHandler())
		req, err := http.NewRequest("GET", server.URL+"/api/v1/operator-credentials?token="+url.QueryEscape(key), nil)
		if err != nil {
			t.Fatal(err)
		}
		// Upgrade headers must not turn absent/whitespace material into an admin
		// credential, even on an ordinary management endpoint.
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		response, err := server.Client().Do(req)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		server.Close()
		if response.StatusCode != 401 {
			t.Fatalf("blank configuration granted administrator status=%d", response.StatusCode)
		}
	}
}
func TestEmptyBearerNeverAuthenticates(t *testing.T) {
	a := NewAPIKeyAuthenticator("")
	r := httptest.NewRequest("GET", "/api/v1/operator-credentials", nil)
	r.Header.Set("Authorization", "Bearer ")
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("empty bearer authenticated")
	}
}

func TestBlankScopedMaterialIsNotAccepted(t *testing.T) {
	if _, err := ParseScopedCallers(`[{"name":"bad-material","key":"   ","scopes":["metrics:read"]}]`); err == nil {
		t.Fatal("blank inline caller key accepted")
	}
	a := NewAPIKeyAuthenticator("valid-root-fixture", ScopedCaller{Name: "blank-scoped", Key: " ", Scopes: []string{"metrics:read"}})
	req := httptest.NewRequest("GET", "/api/v1/version?token=+", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("blank scoped key authenticated")
	}
}
func TestSettingBlankRootMaterialRemovesAdministratorAccess(t *testing.T) {
	a := NewAPIKeyAuthenticator("valid-root-fixture")
	a.SetKey(" \t\n")
	if a.currentKey() != "" {
		t.Fatal("blank root material remains configured")
	}
}
