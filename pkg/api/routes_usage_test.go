package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/usagestore"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestUsageReadRequiresScopeAndCurrentAgentResource(t *testing.T) {
	agent := bareAgent("usage-agent")
	store := &usagestore.PostgresStore{} // Authorization must fail before any storage call.
	server := &api.Server{K8sClient: fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent).Build(),
		APIKey: testAPIKey, Namespace: "kyber-system", UsageStore: store, Callers: []api.ScopedCaller{
			{Name: "reader", Key: "usage-reader", PrincipalID: "principal_reader", TenantID: "tenant_test", CredentialID: "credential_reader", CredentialGeneration: 1, AgentResources: []string{"kyber-system/usage-agent"}, Scopes: []string{"usage:read"}},
			{Name: "other", Key: "usage-other", PrincipalID: "principal_other", TenantID: "tenant_test", CredentialID: "credential_other", CredentialGeneration: 1, AgentResources: []string{"kyber-system/other"}, Scopes: []string{"usage:read"}},
			{Name: "no-scope", Key: "usage-no-scope", Scopes: []string{"requests:read"}},
		}}
	h := server.BuildHandler()
	path := "/api/v1/usage/events?agent=usage-agent&start=2026-10-10T00:00:00Z&end=2026-10-11T00:00:00Z"
	request := func(key, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := request("usage-no-scope", path); rr.Code != http.StatusForbidden {
		t.Fatalf("scope=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("usage-other", path); rr.Code != http.StatusNotFound {
		t.Fatalf("resource=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("usage-reader", path+"&cursor=not-a-cursor"); rr.Code != http.StatusBadRequest {
		t.Fatalf("cursor=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("usage-reader", "/api/v1/usage/aggregates?agent=usage-agent&start=2026-10-10&end=2026-10-11"); rr.Code != http.StatusBadRequest {
		t.Fatalf("bucket=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("usage-other", "/api/v1/usage/events?start=2026-10-10T00:00:00Z&end=2026-10-11T00:00:00Z"); rr.Code != http.StatusOK {
		t.Fatalf("empty authorized set=%d %s", rr.Code, rr.Body.String())
	}
}

func TestUsageReadWithoutPostgresIsUnavailable(t *testing.T) {
	server := &api.Server{APIKey: testAPIKey}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage/events?start=2026-10-10T00:00:00Z&end=2026-10-11T00:00:00Z", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	server.BuildHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("without Postgres=%d %s", rr.Code, rr.Body.String())
	}
}
