package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAdoptionDatabaseProfileUsesStartupOrderAndPrivateReferences(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kyber-system", Name: "database-source"}, Data: map[string][]byte{"password": []byte("pass-test")}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	container := corev1.Container{Env: []corev1.EnvVar{{Name: "POSTGRES_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: "password"}}}, {Name: "KYBER_POSTGRES_URL", Value: "postgres://user:$(POSTGRES_PASSWORD)@db/kyber?sslmode=disable"}, {Name: "POSTGRES_PASSWORD", Value: "later-test"}}}
	value, err := readAdoptionDatabaseURL(context.Background(), reader, "kyber-system", container)
	if err != nil || value.value != "postgres://user:pass-test@db/kyber?sslmode=disable" {
		t.Fatal("database profile diverged from startup environment order", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%q", "%x"} {
		output := fmt.Sprintf(format, value)
		if strings.Contains(output, "pass-test") || strings.Contains(output, "postgres://") || !strings.Contains(output, "redacted") {
			t.Fatal("database profile exposed credentials")
		}
	}
	if _, err := json.Marshal(value); !errors.Is(err, operatorcredentials.ErrSensitive) {
		t.Fatal("database profile serialized")
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: "KYBER_POSTGRES_URL", Value: "postgresql://external/db?sslmode=require"})
	if value, err := readAdoptionDatabaseURL(context.Background(), reader, "kyber-system", container); err != nil || value.value != "postgresql://external/db?sslmode=require" {
		t.Fatal("external URL override ignored", err)
	}
}
func TestAdoptionDatabaseProfileRejectsUnknownOrUnavailableInputs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	for _, env := range [][]corev1.EnvVar{
		nil,
		{{Name: "PGSSLMODE", Value: "disable"}, {Name: "KYBER_POSTGRES_URL", Value: "postgres://db/name"}},
		{{Name: "KYBER_POSTGRES_URL", Value: "postgres://user:$(POSTGRES_PASSWORD)@db/name"}},
		{{Name: "KYBER_POSTGRES_URL", Value: "postgres://$(UNKNOWN)/name"}},
		{{Name: "KYBER_POSTGRES_URL", Value: "https://other/service"}},
		{{Name: "KYBER_POSTGRES_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "url"}}}},
	} {
		if _, err := readAdoptionDatabaseURL(context.Background(), reader, "kyber-system", corev1.Container{Env: env}); !errors.Is(err, operatorcredentials.ErrUnavailable) {
			t.Fatal("unsupported or unavailable database profile accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readAdoptionDatabaseURL(ctx, reader, "kyber-system", corev1.Container{Env: []corev1.EnvVar{{Name: "KYBER_POSTGRES_URL", Value: "postgres://db/name"}}}); !errors.Is(err, operatorcredentials.ErrUnavailable) {
		t.Fatal("cancelled profile accepted")
	}
}
