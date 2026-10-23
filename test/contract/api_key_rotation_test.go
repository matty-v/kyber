//go:build integration

package contract

import (
	"net/http"
	"testing"

	"github.com/matty-v/kyber/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestContract_InstallationKeyRotation(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		name := "administrator"
		if restricted {
			name = "restricted"
		}
		t.Run(name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-key", Namespace: contractNS}, Data: map[string][]byte{"api-key": []byte(contractAPIKey)}}
			s := &api.Server{
				APIKey: contractAPIKey, APIKeySecretName: secret.Name, Namespace: contractNS,
				K8sClient: fake.NewClientBuilder().WithScheme(newContractScheme(t)).WithObjects(secret).Build(),
				Callers:   []api.ScopedCaller{{Name: "integration", Key: "restricted-contract-key", Scopes: []string{"lifecycle:admin"}}},
			}
			req := authedReq(t, http.MethodPost, "/api/v1/rotate-api-key", nil)
			want := http.StatusOK
			if restricted {
				req.Header.Set("Authorization", "Bearer restricted-contract-key")
				want = http.StatusForbidden
			}
			r := validateContract(t, s.BuildHandler(), req)
			if r.Code != want {
				t.Fatalf("rotation status = %d, want %d", r.Code, want)
			}
		})
	}
}
