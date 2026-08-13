package selfupgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestImageTag(t *testing.T) {
	for _, tc := range []struct{ image, want string }{
		{"ghcr.io/matty-v/kyber-control-plane:1.0.2", "1.0.2"},
		{"ghcr.io/matty-v/kyber-control-plane:1.0.2@sha256:deadbeef", "1.0.2"},
		{"ghcr.io/matty-v/kyber-control-plane@sha256:deadbeef", ""},
		{"ghcr.io/matty-v/kyber-control-plane", ""},
		// A registry port is a colon that is NOT a tag separator.
		{"localhost:5000/kyber-control-plane", ""},
		{"localhost:5000/kyber-control-plane:1.0.2", "1.0.2"},
		{"", ""},
	} {
		if got := imageTag(tc.image); got != tc.want {
			t.Errorf("imageTag(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

func deployment(image string, gen, observed int64, updated, ready, unavailable int32) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "kyber-control-plane", Namespace: "kyber-system", Generation: gen},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "control-plane", Image: image}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  observed,
			UpdatedReplicas:     updated,
			ReadyReplicas:       ready,
			UnavailableReplicas: unavailable,
		},
	}
}

func newVerifier(t *testing.T, dep *appsv1.Deployment, healthCode int) *DeploymentVerifier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(healthCode)
	}))
	t.Cleanup(srv.Close)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(dep).Build()
	return &DeploymentVerifier{
		Client:     c,
		Namespace:  "kyber-system",
		Deployment: "kyber-control-plane",
		HealthURL:  srv.URL + "/healthz",
		Interval:   time.Millisecond,
	}
}

func TestVerify_PassesWhenRolledOutOnTheTargetImage(t *testing.T) {
	v := newVerifier(t, deployment("ghcr.io/matty-v/kyber-control-plane:1.0.2", 2, 2, 1, 1, 0), http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := v.Verify(ctx, "1.0.2"); err != nil {
		t.Fatalf("Verify() = %v, want nil", err)
	}
}

// The check that catches the failure Helm cannot: templates rewritten, images
// untouched. Helm would call that a success.
func TestVerify_FailsWhenTheImageDidNotMove(t *testing.T) {
	v := newVerifier(t, deployment("ghcr.io/matty-v/kyber-control-plane:1.0.1", 2, 2, 1, 1, 0), http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := v.Verify(ctx, "1.0.2")
	if err == nil {
		t.Fatal("Verify() = nil, want a failure — the image is still 1.0.1")
	}
	if !strings.Contains(err.Error(), "1.0.1") || !strings.Contains(err.Error(), "1.0.2") {
		t.Errorf("error should name both what is running and what was wanted, got: %v", err)
	}
}

func TestVerify_FailsWhileRolloutIncomplete(t *testing.T) {
	v := newVerifier(t, deployment("ghcr.io/matty-v/kyber-control-plane:1.0.2", 2, 2, 1, 0, 1), http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := v.Verify(ctx, "1.0.2")
	if err == nil {
		t.Fatal("Verify() = nil, want a failure while replicas are unready")
	}
	if !strings.Contains(err.Error(), "rollout incomplete") {
		t.Errorf("error should name the rollout, got: %v", err)
	}
}

func TestVerify_FailsWhenHealthEndpointIsUnhappy(t *testing.T) {
	v := newVerifier(t, deployment("ghcr.io/matty-v/kyber-control-plane:1.0.2", 2, 2, 1, 1, 0), http.StatusServiceUnavailable)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := v.Verify(ctx, "1.0.2")
	if err == nil {
		t.Fatal("Verify() = nil, want a failure — a scheduled pod that does not serve is not a successful upgrade")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should carry the status code, got: %v", err)
	}
}

func TestVerify_RefusesWithoutAHealthURL(t *testing.T) {
	v := newVerifier(t, deployment("ghcr.io/matty-v/kyber-control-plane:1.0.2", 2, 2, 1, 1, 0), http.StatusOK)
	v.HealthURL = ""
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if err := v.Verify(ctx, "1.0.2"); err == nil {
		t.Fatal("Verify() = nil, want a refusal to pass an unprobed control plane")
	}
}

func TestLoadCRDs_ReadsEveryDocumentInOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "b.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: machines.kyber.io\n")
	write(t, dir, "a.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: agents.kyber.io\n---\n")

	crds, err := LoadCRDs(dir)
	if err != nil {
		t.Fatalf("LoadCRDs() = %v", err)
	}
	if len(crds) != 2 {
		t.Fatalf("LoadCRDs() returned %d CRDs, want 2", len(crds))
	}
	// Sorted by filename, so a.yaml's CRD comes first — a stable order makes
	// the Job log comparable between runs.
	if crds[0].GetName() != "agents.kyber.io" || crds[1].GetName() != "machines.kyber.io" {
		t.Errorf("unexpected order: %s, %s", crds[0].GetName(), crds[1].GetName())
	}
}

// Helm applies crds/ verbatim and never templates it. A non-CRD document in
// there would be silently ignored forever, so it is a packaging error.
func TestLoadCRDs_RejectsNonCRDDocuments(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "oops.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: sneaky\n")

	_, err := LoadCRDs(dir)
	if err == nil {
		t.Fatal("LoadCRDs() = nil, want an error for a non-CRD document")
	}
	if !strings.Contains(err.Error(), "ConfigMap") || !strings.Contains(err.Error(), "sneaky") {
		t.Errorf("error should name the offending object, got: %v", err)
	}
}

func TestLoadCRDs_IgnoresNonYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "README.md", "not yaml")
	write(t, dir, "agents.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: agents.kyber.io\n")

	crds, err := LoadCRDs(dir)
	if err != nil {
		t.Fatalf("LoadCRDs() = %v", err)
	}
	if len(crds) != 1 {
		t.Fatalf("LoadCRDs() returned %d CRDs, want 1", len(crds))
	}
}

func TestLoadCRDs_MissingDirectoryIsAnError(t *testing.T) {
	if _, err := LoadCRDs(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("LoadCRDs() = nil, want an error for a missing directory")
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
