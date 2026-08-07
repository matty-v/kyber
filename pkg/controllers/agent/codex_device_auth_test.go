package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestCodexDeviceAuthPending(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "login", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Runtime: "codex",
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
	}
	tests := []struct {
		name string
		auth string
		want bool
	}{
		{name: "placeholder waits", auth: `{}`, want: true},
		{name: "real credential times out normally", auth: `{"tokens":{}}`, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "login-codex-auth", Namespace: "kyber-system"},
				Data:       map[string][]byte{"auth.json": []byte(tc.auth)},
			}
			r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()}
			got, err := r.codexDeviceAuthPending(context.Background(), agent)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("codexDeviceAuthPending()=%v, want %v", got, tc.want)
			}
		})
	}
}
