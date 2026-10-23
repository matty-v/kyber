//go:build integration

package contract

import (
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestContract_SensitiveCredentialGrants(t *testing.T) {
	for _, tc := range []struct {
		scope, method, path string
		body                interface{}
		want                int
	}{
		{"secrets:list", "GET", "/api/v1/agents/alpha/secrets", nil, 200},
		{"secrets:read", "GET", "/api/v1/agents/alpha/secrets/TOKEN", nil, 200},
		{"secrets:write", "PUT", "/api/v1/agents/alpha/secrets/TOKEN", map[string]string{"value": "replacement-fixture"}, 204},
		{"secrets:write", "DELETE", "/api/v1/agents/alpha/secrets/TOKEN", nil, 204},
		{"secrets:list", "GET", "/api/v1/agents/alpha/secrets/TOKEN", nil, 403},
		{"secrets:read", "GET", "/api/v1/agents/alpha/secrets", nil, 403},
		{"logs:read", "GET", "/api/v1/agents/alpha/logs", nil, 503},
		{"logs:read", "GET", "/api/v1/agents/alpha/logs?source=transcript&since=2026-06-03T10:00:00Z&until=2026-06-03T11:00:00Z", nil, 403},
		{"transcripts:read", "GET", "/api/v1/agents/alpha/logs?source=transcript&since=2026-06-03T10:00:00Z&until=2026-06-03T11:00:00Z", nil, 503},
		{"machines:logs:read", "GET", "/api/v1/machines/worker-1/logs", nil, 503},
		{"logs:read", "GET", "/api/v1/machines/worker-1/logs", nil, 403},
	} {
		t.Run(tc.scope+" "+tc.method+" "+tc.path, func(t *testing.T) {
			agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: contractNS}, Spec: kyberv1.AgentSpec{Runtime: "claude-code"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseStopped}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alpha-user-secrets-kv", Namespace: contractNS}, Data: map[string][]byte{"TOKEN": []byte("contract-value-fixture")}}
			machine := &kyberv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: contractNS}}
			c := fake.NewClientBuilder().WithScheme(newContractScheme(t)).WithObjects(agent, secret, machine).Build()
			srv := &api.Server{APIKey: contractAPIKey, Namespace: contractNS, K8sClient: c, Callers: []api.ScopedCaller{{Name: "integration", Key: "contract-sensitive-integration", PrincipalID: "principal_contract", TenantID: "tenant_contract", CredentialID: "credential_contract", CredentialGeneration: 1, Scopes: []string{tc.scope}, AgentResources: []string{contractNS + "/alpha"}, MachineResources: []string{contractNS + "/worker-1"}}}}
			req := authedReq(t, tc.method, tc.path, tc.body)
			req.Header.Set("Authorization", "Bearer contract-sensitive-integration")
			res := validateContract(t, srv.BuildHandler(), req)
			if res.Code != tc.want {
				t.Fatalf("status %d, want %d", res.Code, tc.want)
			}
			if res.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("sensitive response can be cached")
			}
		})
	}
}
