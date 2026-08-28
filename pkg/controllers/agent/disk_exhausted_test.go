package agent

import (
	"context"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClassifyEvent_DiskReserveLifecycle(t *testing.T) {
	r := &AgentReconciler{}
	agent := &kyberv1.Agent{
		Spec: kyberv1.AgentSpec{DesiredPhase: kyberv1.AgentPhaseRunning},
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseRunning,
			Activity: &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{
				DiskReserveReached: true,
			}},
		},
	}

	event, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil || event != EventDiskReserveReached {
		t.Fatalf("reserve reached: event=%q err=%v, want %q", event, err, EventDiskReserveReached)
	}

	agent.Status.Phase = kyberv1.AgentPhaseDiskExhausted
	agent.Status.Activity.Resources.DiskReserveReached = false
	event, err = r.classifyEvent(context.Background(), agent, nil)
	if err != nil || event != EventDiskReserveCleared {
		t.Fatalf("reserve cleared: event=%q err=%v, want %q", event, err, EventDiskReserveCleared)
	}
}

func TestEnsureDiskRecoveryCapacity_ExpandsBeforeRecovery(t *testing.T) {
	ctx := context.Background()
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			DesiredPhase: kyberv1.AgentPhaseRunning,
			Resources:    kyberv1.AgentResources{Disk: resource.MustParse("20Gi")},
		},
		Status: kyberv1.AgentStatus{
			Phase:         kyberv1.AgentPhaseDiskExhausted,
			RecoveryInput: "disk=10Gi",
			Activity: &kyberv1.ActivityStatus{Resources: &kyberv1.AgentResourceUsage{
				DiskReserveReached: true,
			}},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: PVCName(agent.Name), Namespace: agent.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		}},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		},
	}
	c := fake.NewClientBuilder().WithScheme(buildTestScheme()).WithStatusSubresource(agent, pvc).WithObjects(agent, pvc).Build()
	r := &AgentReconciler{Client: c}

	ready, err := r.ensureDiskRecoveryCapacity(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("recovery became ready before PVC capacity expanded")
	}
	event, err := r.classifyEvent(ctx, agent, nil)
	if err != nil || event != "" {
		t.Fatalf("before capacity expansion: event=%q err=%v, want no recovery", event, err)
	}
	stored := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}
	if err := c.Get(ctx, key, stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Fatalf("PVC request = %s, want 20Gi", got.String())
	}

	stored.Status.Capacity[corev1.ResourceStorage] = resource.MustParse("20Gi")
	if err := c.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	ready, err = r.ensureDiskRecoveryCapacity(ctx, agent)
	if err != nil || !ready {
		t.Fatalf("expanded PVC: ready=%v err=%v, want true", ready, err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, agent); err != nil {
		t.Fatal(err)
	}
	event, err = r.classifyEvent(ctx, agent, nil)
	if err != nil || event != EventDesiredRunning {
		t.Fatalf("after capacity expansion: event=%q err=%v, want %q", event, err, EventDesiredRunning)
	}
}
