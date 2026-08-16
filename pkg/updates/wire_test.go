package updates

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestApplierStart_ConsultsTheNodePreflight guards the WIRING rather than the
// checker. CheckNodeCapability has its own tests; this one exists so that
// removing the call from Start — which would restore exactly the failure mode
// the preflight was written for, an upgrade that lands and stops every agent —
// cannot pass silently.
func TestApplierStart_ConsultsTheNodePreflight(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	// A Helm-owned control plane, so ownership detection (which runs first)
	// lets us reach the preflight at all.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "kyber-control-plane",
			Namespace:   "kyber-system",
			Annotations: map[string]string{"meta.helm.sh/release-name": "kyber"},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "control-plane", Image: "ghcr.io/matty-v/kyber-control-plane:v1.0.5",
				}}},
			},
		},
	}
	// ...on a cluster too old to run agents.
	old := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "old"},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			KubeletVersion:          "v1.31.5+k3s1",
			ContainerRuntimeVersion: "containerd://1.7.23-k3s2",
		}},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(dep, old).Build()
	a := &Applier{
		Client:                 c,
		Namespace:              "kyber-system",
		ControlPlaneDeployment: "kyber-control-plane",
		ReleaseName:            "kyber",
		ChartRef:               "oci://ghcr.io/matty-v/charts/kyber",
		ServiceAccount:         "kyber-upgrade",
	}

	_, err := a.Start(context.Background(), "1.2.3", Policy{Channel: ChannelStable})
	if err == nil {
		t.Fatal("Start accepted an upgrade onto a cluster that cannot run agents")
	}
	if !strings.Contains(err.Error(), "cannot run agents") {
		t.Fatalf("Start refused for the wrong reason — the node preflight may not be wired in: %v", err)
	}
}
