//go:build integration

package api

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reads the existing dev sources only. No test credential, Agent, candidate,
// registry, marker or maintenance object is created or changed.
func TestGCPDevCredentialAdoptionSources(t *testing.T) {
	if os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_VERIFY") != "1" {
		t.Skip("explicit GCP dev verification required")
	}
	const namespace = "kyber-system"
	const host = "https://136.119.30.26"
	if os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_HOST") != host || os.Getenv("KUBECONFIG") == "" {
		t.Fatal("verified dev endpoint/configuration required")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")}, &clientcmd.ConfigOverrides{CurrentContext: "gke_datawire-dev_us-central1-a_kyber-dev"}).ClientConfig()
	if err != nil || cfg.Host != host {
		t.Fatal("dev context/endpoint mismatch")
	}
	cfg.Timeout = 5 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, kyberv1.AddToScheme} {
		if add(scheme) != nil {
			t.Fatal("verification scheme unavailable")
		}
	}
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("verification client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var agents kyberv1.AgentList
	if err := direct.List(ctx, &agents); err != nil || len(agents.Items) != 0 {
		t.Fatal("zero Agents required cluster-wide")
	}
	deployment := &appsv1.Deployment{}
	if err := direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "kyber-dev-control-plane"}, deployment); err != nil || deployment.Labels["app.kubernetes.io/instance"] != "kyber-dev" {
		t.Fatal("dev release mismatch")
	}
	key := client.ObjectKey{Namespace: namespace, Name: "kyber-api-credentials"}
	before := &corev1.Secret{}
	if err := direct.Get(ctx, key, before); err != nil {
		t.Fatal("source preflight unavailable")
	}
	reader, err := newCredentialAdoptionSourceReader(cfg, namespace, key.Name)
	if err != nil {
		t.Fatal("source reader unavailable")
	}
	snapshot, err := reader.Read(ctx)
	if err != nil {
		t.Fatal("source snapshot unverified")
	}
	if snapshot.root != string(before.Data[apiKeySecretField]) || len(snapshot.versions) < 1 {
		t.Fatal("snapshot did not preserve source")
	}
	if err := snapshot.Verify(ctx); err != nil {
		t.Fatal("source freshness unverified")
	}
	after := &corev1.Secret{}
	if err := direct.Get(ctx, key, after); err != nil || after.UID != before.UID || after.ResourceVersion != before.ResourceVersion || !reflect.DeepEqual(after.Data, before.Data) {
		t.Fatal("source changed")
	}
	if err := direct.List(ctx, &agents); err != nil || len(agents.Items) != 0 {
		t.Fatal("Agents appeared during verification")
	}
	current := &appsv1.Deployment{}
	if err := direct.Get(ctx, client.ObjectKeyFromObject(deployment), current); err != nil || !reflect.DeepEqual(current.Spec.Template, deployment.Spec.Template) {
		t.Fatal("workload changed")
	}
	t.Log("Direct source snapshot and recheck passed; values, metadata and workload unchanged; zero Agents cluster-wide")
}
