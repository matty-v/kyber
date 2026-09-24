package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
)

// MAT-90 G11: an API-key agent created with no key used to be accepted, then
// sat in Creating forever on a pod referencing a Secret nobody would write.
func createWithSecrets(t *testing.T, runtime, secrets string, objs ...client.Object) *httptest.ResponseRecorder {
	t.Helper()
	objs = append(objs, defaultMachine())
	server := &api.Server{
		K8sClient:     fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(objs...).Build(),
		APIKey:        testAPIKey,
		Namespace:     "kyber-system",
		ValidRuntimes: map[string]bool{runtime: true},
	}
	body := `{"name":"keyless","machine":"worker-1","runtime":"` + runtime + `","secrets":` + secrets + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	server.BuildHandler().ServeHTTP(rr, req)
	return rr
}

func TestCreateRejectsAPIKeyModeWithoutKey(t *testing.T) {
	for _, tc := range []struct{ runtime, secrets, field string }{
		{"claude-code", `{"authType":"api-key"}`, "secrets.anthropicApiKey"},
		{"claude-code", `{"authType":"api-key","anthropicApiKey":"  "}`, "secrets.anthropicApiKey"},
		{"hermes", `{"authType":"api-key"}`, "openrouterApiKey"},
	} {
		rr := createWithSecrets(t, tc.runtime, tc.secrets)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.field) {
			t.Fatalf("%s %s: status=%d body=%s, want 400 naming %s", tc.runtime, tc.secrets, rr.Code, rr.Body.String(), tc.field)
		}
	}
}

func TestCreateAcceptsAPIKeyModeWithProvisionedSecret(t *testing.T) {
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keyless-anthropic", Namespace: "kyber-system"},
		Data: map[string][]byte{"token": []byte("sk-provisioned")}}
	rr := createWithSecrets(t, "claude-code", `{"authType":"api-key"}`, existing)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201 for a pre-provisioned credential", rr.Code, rr.Body.String())
	}
}

func TestCreateOAuthWithoutCodeStillAccepted(t *testing.T) {
	rr := createWithSecrets(t, "claude-code", `{"authType":"oauth"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201 (OAuth authorizes after create)", rr.Code, rr.Body.String())
	}
}
