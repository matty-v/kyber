//go:build integration

package contract

import (
	"net/http"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
)

func TestContract_CurrentOperatorCredential(t *testing.T) {
	s := &api.Server{APIKey: contractAPIKey, Namespace: contractNS, OperatorCredentialPodUID: "6125a706-7020-4e22-b6bd-1de5ebf6fb10", Callers: []api.ScopedCaller{{Key: "current-contract-client", Name: "Current integration", PrincipalID: "principal-current", TenantID: "tenant-current", CredentialID: "credential-current", CredentialGeneration: 1, Scopes: []string{"metrics:read"}, AgentResources: []string{"kyber-system/alpha"}}, {Key: "unknown-contract-client", Name: "Unknown identity", Scopes: []string{}}}}
	handler := s.BuildHandler()
	for _, key := range []string{contractAPIKey, "current-contract-client", "unknown-contract-client"} {
		request := authedReq(t, http.MethodGet, "/api/v1/operator-credentials/current", nil)
		request.Header.Set("Authorization", "Bearer "+key)
		response := validateContract(t, handler, request)
		if response.Code != 200 {
			t.Fatal("own current metadata unavailable", response.Code)
		}
	}
	request := authedReq(t, http.MethodGet, "/api/v1/operator-credentials/current", nil)
	request.Header.Del("Authorization")
	if response := validateContract(t, handler, request); response.Code != 401 {
		t.Fatal("unauthenticated metadata read admitted", response.Code)
	}
}
