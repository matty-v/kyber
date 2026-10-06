package agent

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func setupNodeRecoveryTest(t *testing.T) (client.Client, *AgentReconciler, types.NamespacedName, func()) {
	t.Helper()
	k8sClient, teardown := setupEnvtest(t)
	ctx := context.Background()
	namespace := "test-node-recovery"
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		teardown()
		t.Fatalf("creating namespace: %v", err)
	}
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: namespace},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderFake, DesiredPhase: kyberv1.MachinePhaseRunning,
		},
	}
	if err := k8sClient.Create(ctx, machine); err != nil {
		teardown()
		t.Fatalf("creating machine: %v", err)
	}
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.Phase = kyberv1.MachinePhaseReady
	machine.Status.NodeName = "old-node"
	if err := k8sClient.Status().Patch(ctx, machine, patch); err != nil {
		teardown()
		t.Fatalf("setting machine Ready: %v", err)
	}
	agent := newTestAgent("dave", namespace)
	agent.Spec.Runtime = "claude-code"
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := k8sClient.Create(ctx, agent); err != nil {
		teardown()
		t.Fatalf("creating agent: %v", err)
	}
	r := newReconciler(k8sClient, buildTestScheme())
	r.MachineGetter = &KubernetesMachineGetter{Client: k8sClient}
	key := types.NamespacedName{Name: agent.Name, Namespace: namespace}
	reconcileN(t, r, ctrl.Request{NamespacedName: key}, 1)
	return k8sClient, r, key, teardown
}

func setNodeRecoveryAgentFailed(t *testing.T, c client.Client, key types.NamespacedName) {
	t.Helper()
	agent := getAgent(t, c, key)
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.Phase = kyberv1.AgentPhaseFailed
	agent.Status.RestartCount = 1
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	agent.Status.LastTransition = &past
	if err := c.Status().Patch(context.Background(), agent, patch); err != nil {
		t.Fatalf("setting agent Failed: %v", err)
	}
}

func setNodeRecoveryMachine(t *testing.T, c client.Client, namespace string, phase kyberv1.MachinePhase, node string) {
	t.Helper()
	ctx := context.Background()
	machine := &kyberv1.Machine{}
	if err := c.Get(ctx, types.NamespacedName{Name: "node-01", Namespace: namespace}, machine); err != nil {
		t.Fatalf("getting machine: %v", err)
	}
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Status.Phase = phase
	machine.Status.NodeName = node
	if err := c.Status().Patch(ctx, machine, patch); err != nil {
		t.Fatalf("updating machine: %v", err)
	}
}

func TestReconciler_FailedAgentWaitsForUnavailableMachine(t *testing.T) {
	c, r, key, teardown := setupNodeRecoveryTest(t)
	defer teardown()
	setNodeRecoveryAgentFailed(t, c, key)
	setNodeRecoveryMachine(t, c, key.Namespace, kyberv1.MachinePhaseProvisioning, "old-node")

	req := ctrl.Request{NamespacedName: key}
	reconcileN(t, r, req, 1)
	parked := getAgent(t, c, key)
	if parked.Status.Phase != kyberv1.AgentPhaseWaitingForMachine {
		t.Fatalf("phase = %q, want WaitingForMachine", parked.Status.Phase)
	}
	if parked.Status.RestartCount != 1 {
		t.Errorf("restart count = %d, want unchanged 1", parked.Status.RestartCount)
	}
	podKey := types.NamespacedName{Name: AgentPodName(key.Name), Namespace: key.Namespace}
	if err := c.Get(context.Background(), podKey, &corev1.Pod{}); !errors.IsNotFound(err) {
		t.Fatalf("old pod still exists after parking: %v", err)
	}

	setNodeRecoveryMachine(t, c, key.Namespace, kyberv1.MachinePhaseReady, "new-node")
	reconcileN(t, r, req, 1)
	resumed := getAgent(t, c, key)
	if resumed.Status.Phase != kyberv1.AgentPhaseCreating {
		t.Fatalf("phase = %q, want Creating", resumed.Status.Phase)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), podKey, pod); err != nil {
		t.Fatalf("getting replacement pod: %v", err)
	}
	if got := podRequiredHostname(pod); got != "new-node" {
		t.Errorf("replacement hostname = %q, want new-node", got)
	}
}

func TestReconciler_FailedAgentReplacesPendingPodPinnedToOldNode(t *testing.T) {
	c, r, key, teardown := setupNodeRecoveryTest(t)
	defer teardown()
	ctx := context.Background()
	podKey := types.NamespacedName{Name: AgentPodName(key.Name), Namespace: key.Namespace}
	oldPod := &corev1.Pod{}
	if err := c.Get(ctx, podKey, oldPod); err != nil {
		t.Fatalf("getting old pod: %v", err)
	}
	pvcKey := types.NamespacedName{Name: PVCName(key.Name), Namespace: key.Namespace}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, pvcKey, pvc); err != nil {
		t.Fatalf("getting agent PVC: %v", err)
	}
	pvcUID := pvc.UID
	patch := client.MergeFrom(oldPod.DeepCopy())
	oldPod.Status.Phase = corev1.PodPending
	oldPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
	if err := c.Status().Patch(ctx, oldPod, patch); err != nil {
		t.Fatalf("marking old pod unschedulable: %v", err)
	}
	setNodeRecoveryAgentFailed(t, c, key)
	setNodeRecoveryMachine(t, c, key.Namespace, kyberv1.MachinePhaseReady, "new-node")
	// Infrastructure recovery must work even if earlier crashes used the
	// ordinary retry budget.
	agent := getAgent(t, c, key)
	statusPatch := client.MergeFrom(agent.DeepCopy())
	agent.Status.RestartCount = maxRestartRetries
	if err := c.Status().Patch(ctx, agent, statusPatch); err != nil {
		t.Fatalf("setting exhausted crash retry count: %v", err)
	}

	req := ctrl.Request{NamespacedName: key}
	reconcileN(t, r, req, 1)
	if phase := getAgent(t, c, key).Status.Phase; phase != kyberv1.AgentPhaseWaitingForMachine {
		t.Fatalf("phase after clearing stale pod = %q, want WaitingForMachine", phase)
	}
	if err := c.Get(ctx, podKey, &corev1.Pod{}); !errors.IsNotFound(err) {
		t.Fatalf("stale Pending pod still exists: %v", err)
	}
	reconcileN(t, r, req, 1)
	if phase := getAgent(t, c, key).Status.Phase; phase != kyberv1.AgentPhaseCreating {
		t.Fatalf("phase after replacement = %q, want Creating", phase)
	}
	replacement := &corev1.Pod{}
	if err := c.Get(ctx, podKey, replacement); err != nil {
		t.Fatalf("getting replacement pod: %v", err)
	}
	if replacement.UID == oldPod.UID || podRequiredHostname(replacement) != "new-node" {
		t.Errorf("replacement pod uid/hostname = %q/%q, want new UID/new-node", replacement.UID, podRequiredHostname(replacement))
	}
	if err := c.Get(ctx, pvcKey, pvc); err != nil || pvc.UID != pvcUID {
		t.Errorf("PVC changed during pod replacement: uid=%q, err=%v", pvc.UID, err)
	}
	statusPatch = client.MergeFrom(replacement.DeepCopy())
	replacement.Status.Phase = corev1.PodRunning
	replacement.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := c.Status().Patch(ctx, replacement, statusPatch); err != nil {
		t.Fatalf("marking replacement pod Ready: %v", err)
	}
	reconcileN(t, r, req, 2)
	if phase := getAgent(t, c, key).Status.Phase; phase != kyberv1.AgentPhaseRunning {
		t.Errorf("phase after replacement pod became Ready = %q, want Running", phase)
	}
}

func TestStaleNodePendingPodRequiresHostnameDrift(t *testing.T) {
	c, r, key, teardown := setupNodeRecoveryTest(t)
	defer teardown()
	ctx := context.Background()
	agent := getAgent(t, c, key)
	pod := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Name: AgentPodName(key.Name), Namespace: key.Namespace}, pod); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
	if r.isStaleNodePendingPod(ctx, agent, pod, "old-node") {
		t.Error("same-hostname Pending pod was considered stale")
	}
	pod.Status.Phase = corev1.PodRunning
	if r.isStaleNodePendingPod(ctx, agent, pod, "new-node") {
		t.Error("Running pod was considered stale")
	}
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = nil
	if r.isStaleNodePendingPod(ctx, agent, pod, "new-node") {
		t.Error("Pending pod without an Unschedulable condition was considered stale")
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
	pod.OwnerReferences = nil
	if r.isStaleNodePendingPod(ctx, agent, pod, "new-node") {
		t.Error("pod without Agent ownership was considered stale")
	}
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(agent, kyberv1.GroupVersion.WithKind("Agent"))}
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	agent.Status.Phase = kyberv1.AgentPhaseFailed
	event, err := r.classifyEvent(ctx, agent, pod)
	if err != nil || event != EventDesiredStopped {
		t.Errorf("operator Stop precedence: event=%q err=%v, want DesiredStopped", event, err)
	}
}
