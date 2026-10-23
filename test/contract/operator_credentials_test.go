//go:build integration

package contract

import (
	"net/http"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
)

func TestContract_OperatorCredentialInventory(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		s := &api.Server{APIKey: contractAPIKey, Namespace: contractNS, Callers: []api.ScopedCaller{{Name: "integration", Key: "inventory-contract-restricted"}}}
		req := authedReq(t, http.MethodGet, "/api/v1/operator-credentials", nil)
		want := http.StatusOK
		if restricted {
			req.Header.Set("Authorization", "Bearer inventory-contract-restricted")
			want = http.StatusForbidden
		}
		res := validateContract(t, s.BuildHandler(), req)
		if res.Code != want {
			t.Fatalf("restricted=%v inventory status %d, want %d", restricted, res.Code, want)
		}
	}
}
