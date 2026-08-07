package api

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNodeAvailableResources(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}

	// System pod using 500m CPU and 512Mi memory.
	systemPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{{
				Name: "coredns",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	// Agent pod using 500m CPU and 2Gi memory.
	agentPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-bob", Namespace: "kyber-system"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{{
				Name: "agent",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	// Terminal pod — should be excluded from resource sum.
	completedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "job-done", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Containers: []corev1.Container{{
				Name: "worker",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}

	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, systemPod, agentPod, completedPod).
		Build()

	avail, err := nodeAvailableResources(context.Background(), k8s, "test-node")
	if err != nil {
		t.Fatalf("nodeAvailableResources: %v", err)
	}

	// Expected: allocatable(2000m) - system(500m) - agent(500m) = 1000m
	wantCPU := resource.MustParse("1")
	if avail.CPU.Cmp(wantCPU) != 0 {
		t.Errorf("available CPU: got %s, want %s", avail.CPU.String(), wantCPU.String())
	}

	// Expected: allocatable(8Gi) - system(512Mi) - agent(2Gi) = ~5.5Gi
	wantMem := resource.MustParse("5632Mi") // 8Gi - 512Mi - 2Gi = 5632Mi
	if avail.Memory.Cmp(wantMem) != 0 {
		t.Errorf("available memory: got %s, want %s", avail.Memory.String(), wantMem.String())
	}
}

func TestNodeAvailableResources_NodeNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := nodeAvailableResources(context.Background(), k8s, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent node, got nil")
	}
}
