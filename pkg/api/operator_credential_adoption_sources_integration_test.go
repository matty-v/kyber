//go:build integration

package api

import (
	"context"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reads the existing dev sources only. No test credential, Agent, candidate,
// registry, marker or maintenance object is created or changed.
func TestGCPDevCredentialAdoptionSources(t *testing.T) {
	verifyGCPDevCredentialAdoptionSources(t, false)
}

func TestGCPDevCredentialAdoptionReplica(t *testing.T) {
	verifyGCPDevCredentialAdoptionSources(t, true)
}

func verifyGCPDevCredentialAdoptionSources(t *testing.T, verifyReplicas bool) {
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
	var replicaSet []string
	readReplicas := func() []corev1.Pod {
		t.Helper()
		selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		if err != nil || selector.Empty() {
			t.Fatal("dev replica selector unavailable")
		}
		var pods corev1.PodList
		if err := direct.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil || len(pods.Items) == 0 {
			t.Fatal("dev replicas unavailable")
		}
		return pods.Items
	}
	identities := func(pods []corev1.Pod) []string {
		result := make([]string, 0, len(pods))
		for _, pod := range pods {
			result = append(result, pod.Name+":"+string(pod.UID)+":"+pod.Status.PodIP)
		}
		sort.Strings(result)
		return result
	}
	if verifyReplicas {
		pods := readReplicas()
		replicaSet = identities(pods)
		probe := newCredentialAdoptionReplicaProbe()
		// The investigator may run in a different GKE Pod network. The
		// authenticated test-only forward must pin one exact dev Pod; it is
		// never a Service or a runtime probe transport fallback.
		if forward := os.Getenv("KYBER_OPERATOR_CREDENTIAL_REPLICA_FORWARD"); forward != "" {
			host, port, err := net.SplitHostPort(forward)
			if err != nil || host != "127.0.0.1" || port == "" || len(pods) != 1 || os.Getenv("KYBER_OPERATOR_CREDENTIAL_REPLICA_POD_UID") != string(pods[0].UID) {
				t.Fatal("exact owned dev Pod forward required")
			}
			expected := net.JoinHostPort(pods[0].Status.PodIP, "8080")
			probe.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != expected {
					return nil, errCredentialAdoptionReplica
				}
				return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, forward)
			}
		}
		for _, pod := range pods {
			if err := probe.verify(ctx, snapshot, pod, 8080); err != nil {
				t.Fatal("loaded dev replica credentials unverified")
			}
		}
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
	if verifyReplicas && !reflect.DeepEqual(replicaSet, identities(readReplicas())) {
		t.Fatal("dev replicas changed during verification")
	}
	t.Log("Direct source snapshot and recheck passed; values, metadata and workload unchanged; zero Agents cluster-wide")
}
