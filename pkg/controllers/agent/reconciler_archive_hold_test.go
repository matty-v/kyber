package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// A new agent created from a disk archive is held in Creating with no pod
// until the restore job releases it, and then boots normally (MAT-88).
func TestArchiveHoldGatesFirstPodUntilReleased(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()
	ctx := context.Background()
	namespace := "test-archive-hold"
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	r := newReconciler(k8sClient, buildTestScheme())
	agent := newTestAgent("restored", namespace)
	agent.Annotations = map[string]string{kyberv1.AnnotationArchiveHold: "job123"}
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Name: agent.Name, Namespace: namespace}
	req := ctrl.Request{NamespacedName: key}
	reconcileN(t, r, req, 2)

	got := getAgent(t, k8sClient, key)
	if got.Status.Phase != kyberv1.AgentPhaseCreating || !meta.IsStatusConditionTrue(got.Status.Conditions, kyberv1.AgentConditionAwaitingRestore) {
		t.Fatalf("held agent: phase=%s conditions=%+v", got.Status.Phase, got.Status.Conditions)
	}
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: "agent-" + agent.Name, Namespace: namespace}
	if err := k8sClient.Get(ctx, podKey, pod); !k8serrors.IsNotFound(err) {
		t.Fatalf("pod created while the archive hold was present: %v", err)
	}

	before := got.DeepCopy()
	delete(got.Annotations, kyberv1.AnnotationArchiveHold)
	if err := k8sClient.Patch(ctx, got, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, req, 1)
	if err := k8sClient.Get(ctx, podKey, pod); err != nil {
		t.Fatalf("pod not created after the hold was released: %v", err)
	}
	if got := getAgent(t, k8sClient, key); meta.IsStatusConditionTrue(got.Status.Conditions, kyberv1.AgentConditionAwaitingRestore) {
		t.Error("AwaitingRestore not cleared after the first pod was created")
	}
}

// An existing agent held by an export is never given a pod, even when
// desiredPhase asks for one (MAT-87).
func TestArchiveHoldBlocksRestartOfExistingAgent(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()
	ctx := context.Background()
	namespace := "test-archive-hold-existing"
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	r := newReconciler(k8sClient, buildTestScheme())
	agent := newTestAgent("exported", namespace)
	agent.Annotations = map[string]string{kyberv1.AnnotationArchiveHold: "job456"}
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Name: agent.Name, Namespace: namespace}
	current := getAgent(t, k8sClient, key)
	statusBefore := current.DeepCopy()
	current.Status.Phase = kyberv1.AgentPhaseStopped
	if err := k8sClient.Status().Patch(ctx, current, client.MergeFrom(statusBefore)); err != nil {
		t.Fatal(err)
	}
	// Reconcile reports the refusal as an error (and retries with backoff);
	// what matters is that no pod exists.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "agent-" + agent.Name, Namespace: namespace}, pod); !k8serrors.IsNotFound(err) {
		t.Fatalf("pod created for an agent whose volume is held by an export: %v", err)
	}
}

// Deleting an agent never deletes a same-named volume another object owns.
func TestFinalizerLeavesForeignPVC(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()
	ctx := context.Background()
	namespace := "test-foreign-pvc"
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	r := newReconciler(k8sClient, buildTestScheme())
	other := newTestAgent("other-owner", namespace)
	other.Annotations = map[string]string{kyberv1.AnnotationArchiveHold: "keep-podless"}
	if err := k8sClient.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	agent := newTestAgent("doomed", namespace)
	agent.Annotations = map[string]string{kyberv1.AnnotationArchiveHold: "keep-podless"}
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Name: agent.Name, Namespace: namespace}
	reconcileN(t, r, ctrl.Request{NamespacedName: key}, 1) // adds the finalizer
	foreign := BuildPVC(agent, "")
	foreign.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(getAgent(t, k8sClient, types.NamespacedName{Name: other.Name, Namespace: namespace}), kyberv1.GroupVersion.WithKind("Agent"))}
	if err := k8sClient.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Delete(ctx, getAgent(t, k8sClient, key)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}
	got := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), got); err != nil || got.DeletionTimestamp != nil {
		t.Fatalf("foreign PVC deleted by another agent's finalizer: err=%v deleting=%v", err, got.DeletionTimestamp)
	}
}
