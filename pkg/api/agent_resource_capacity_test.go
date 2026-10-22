package api

import (
	"context"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAgentResourceHeadroomCountsExternalPodsAndSidecars(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{Capacity: kyberv1.MachineCapacity{
			CPU: resource.MustParse("4"), Memory: resource.MustParse("8Gi"),
		}},
		Status: kyberv1.MachineStatus{NodeName: "node-a", Phase: kyberv1.MachinePhaseRunning},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi"),
	}}}
	agent := kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"}, Spec: kyberv1.AgentSpec{
		Machine: "worker", Resources: kyberv1.AgentResources{CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi")},
	}}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-alice", Namespace: "kyber-system", Labels: map[string]string{"kyber.io/agent": "alice"}}, Spec: corev1.PodSpec{
		NodeName: "node-a", Containers: []corev1.Container{
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi")}}},
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")}}},
		},
	}}
	externalPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "other"}, Spec: corev1.PodSpec{
		NodeName: "node-a", Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi"),
		}}}},
	}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, oldPod, externalPod).Build()
	got, err := agentResourceHeadroom(context.Background(), reader, machine, []kyberv1.Agent{agent}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.CPU.Cmp(resource.MustParse("1900m")) != 0 {
		t.Fatalf("CPU headroom=%s, want 1900m after external pod and replacement sidecars", got.CPU.String())
	}
}
