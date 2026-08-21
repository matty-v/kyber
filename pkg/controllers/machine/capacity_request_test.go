package machine

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

type schedulerDemandProvider struct {
	adapters.CapacityProvider
}

func (p *schedulerDemandProvider) Type() string { return "gke" }
func (p *schedulerDemandProvider) NeedsSchedulerDemand() bool {
	return true
}
func (p *schedulerDemandProvider) NodeSelector(adapters.MachineIdentity, adapters.ProviderRef) map[string]string {
	return map[string]string{"cloud.google.com/gke-nodepool": "coders"}
}

func TestCapacityRequestPodLifecycle(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "capacity-request-test"}}
	if err := k8sClient.Create(ctx, namespace); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "coders", Namespace: namespace.Name},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderGKE, ManagementMode: kyberv1.MachineManagementManaged,
			DesiredPhase: kyberv1.MachinePhaseRunning,
		},
	}
	if err := k8sClient.Create(ctx, machine); err != nil {
		t.Fatalf("creating Machine: %v", err)
	}
	machine.Status.Phase = kyberv1.MachinePhaseProvisioning
	if err := k8sClient.Status().Update(ctx, machine); err != nil {
		t.Fatalf("setting Machine status: %v", err)
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sol", Namespace: namespace.Name},
		Spec: kyberv1.AgentSpec{
			Machine: "coders", DesiredPhase: kyberv1.AgentPhaseRunning,
			Scaling: kyberv1.AgentScalingWarm,
			Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
		},
	}
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating Agent: %v", err)
	}

	provider := &schedulerDemandProvider{}
	r := &MachineReconciler{
		Client: k8sClient, Scheme: buildTestScheme(), Recorder: record.NewFakeRecorder(10),
		CapacityProvider: provider,
	}
	if err := r.reconcileCapacityRequestPod(ctx, machine); err != nil {
		t.Fatalf("creating capacity request: %v", err)
	}

	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: capacityRequestPodName(machine.Name), Namespace: namespace.Name}
	if err := k8sClient.Get(ctx, key, pod); err != nil {
		t.Fatalf("getting capacity request Pod: %v", err)
	}
	if got := pod.Spec.NodeSelector["cloud.google.com/gke-nodepool"]; got != "coders" {
		t.Errorf("node pool selector = %q, want coders", got)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("capacity request Pod must disable service-account token automount")
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != machine.UID {
		t.Errorf("ownerReferences = %+v, want Machine UID %s", pod.OwnerReferences, machine.UID)
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseSuspended
	if err := k8sClient.Patch(ctx, agent, patch); err != nil {
		t.Fatalf("suspending Agent: %v", err)
	}
	if err := r.reconcileCapacityRequestPod(ctx, machine); err != nil {
		t.Fatalf("deleting capacity request: %v", err)
	}
	if err := k8sClient.Get(ctx, key, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("capacity request Pod still exists after demand disappeared: %v", err)
	}
}

func TestAgentNeedsMachineCapacity(t *testing.T) {
	tests := []struct {
		name    string
		desired kyberv1.AgentPhase
		phase   kyberv1.AgentPhase
		want    bool
	}{
		{name: "new running", desired: kyberv1.AgentPhaseRunning, want: true},
		{name: "waiting", desired: kyberv1.AgentPhaseRunning, phase: kyberv1.AgentPhaseWaitingForMachine, want: true},
		{name: "wake", desired: kyberv1.AgentPhaseRunning, phase: kyberv1.AgentPhaseSuspended, want: true},
		{name: "suspended", desired: kyberv1.AgentPhaseSuspended, phase: kyberv1.AgentPhaseSuspended, want: false},
		{name: "stopped", desired: kyberv1.AgentPhaseStopped, phase: kyberv1.AgentPhaseStopped, want: false},
		{name: "legacy failed", phase: kyberv1.AgentPhaseFailed, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &kyberv1.Agent{Spec: kyberv1.AgentSpec{DesiredPhase: tt.desired}, Status: kyberv1.AgentStatus{Phase: tt.phase}}
			if got := agentNeedsMachineCapacity(agent); got != tt.want {
				t.Errorf("agentNeedsMachineCapacity() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestEnqueueAgentMachine(t *testing.T) {
	r := &MachineReconciler{}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "sol", Namespace: "kyber-system"},
		Spec:       kyberv1.AgentSpec{Machine: "coders"},
	}
	requests := r.enqueueAgentMachine(context.Background(), agent)
	if len(requests) != 1 || requests[0].Name != "coders" || requests[0].Namespace != "kyber-system" {
		t.Fatalf("requests = %+v, want kyber-system/coders", requests)
	}
}
