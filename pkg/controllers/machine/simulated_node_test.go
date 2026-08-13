package machine

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestManagedNodeReadySimulationGate(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"kyber.io/simulated": "true"}}}
	if (&MachineReconciler{}).isManagedNodeReady(node) {
		t.Fatal("simulated node accepted while development gate disabled")
	}
	if !(&MachineReconciler{AllowSimulatedNodes: true}).isManagedNodeReady(node) {
		t.Fatal("simulated node rejected while development gate enabled")
	}
	real := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if !(&MachineReconciler{}).isManagedNodeReady(real) {
		t.Fatal("real Ready node rejected")
	}
}
