package machine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/adapters"
)

// envtestBinPath resolves the envtest binary path from the KUBEBUILDER_ASSETS env var
// or falls back to the pre-installed 1.31.0 binaries.
func envtestBinPath() string {
	if p := os.Getenv("KUBEBUILDER_ASSETS"); p != "" {
		return p
	}
	return "/tmp/envtest-bin/k8s/1.31.0-linux-amd64"
}

// setupEnvtest creates an envtest environment and returns a configured client and teardown function.
func setupEnvtest(t *testing.T) (client.Client, func()) {
	t.Helper()

	log.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = kyberv1.AddToScheme(scheme)

	crdPath := filepath.Join("..", "..", "..", "deploy", "helm", "kyber", "crds")

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestBinPath(),
		Scheme:                scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		env.Stop() //nolint:errcheck
		t.Fatalf("creating client: %v", err)
	}

	return k8sClient, func() {
		if err := env.Stop(); err != nil {
			t.Logf("stopping envtest: %v", err)
		}
	}
}

// buildTestScheme builds the scheme used in reconciler construction.
func buildTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = kyberv1.AddToScheme(s)
	return s
}

// newReconciler builds a MachineReconciler with a fresh MockComputeAdapter.
// AgentCountFn defaults to returning (0, nil) — no agents — so tests that don't
// exercise agent drain don't need to register a field indexer or seed pods.
// Tests that need specific agent counts should set r.AgentCountFn after calling this.
func newReconciler(k8sClient client.Client, scheme *runtime.Scheme) (*MachineReconciler, *adapters.MockComputeAdapter) {
	adapter := adapters.NewMockComputeAdapter()
	r := &MachineReconciler{
		Client:         k8sClient,
		Scheme:         scheme,
		Recorder:       record.NewFakeRecorder(100),
		ComputeAdapter: adapter,
		AgentCountFn: func(_ context.Context, _ *kyberv1.Machine) (int, error) {
			return 0, nil
		},
	}
	return r, adapter
}

// newTestMachine returns a minimal Machine CRD.
func newTestMachine(name, namespace string) *kyberv1.Machine {
	return &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: kyberv1.MachineSpec{
			Provider:    kyberv1.MachineProviderGCE,
			MachineType: "n2-standard-4",
			DiskSizeGb:  50,
			Spot:        true,
			Zone:        "us-central1-a",
		},
	}
}

// getMachine fetches the Machine from the API server.
func getMachine(t *testing.T, c client.Client, key types.NamespacedName) *kyberv1.Machine {
	t.Helper()
	machine := &kyberv1.Machine{}
	if err := c.Get(context.Background(), key, machine); err != nil {
		t.Fatalf("getting machine: %v", err)
	}
	return machine
}

// reconcileN calls Reconcile n times, stopping early on error.
func reconcileN(t *testing.T, r *MachineReconciler, req ctrl.Request, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d/%d failed: %v", i+1, n, err)
		}
	}
}

// createReadyNode creates a k8s Node that appears Ready and is labeled for the given machine.
func createReadyNode(t *testing.T, c client.Client, machineName string) *corev1.Node {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-" + machineName,
			Labels: map[string]string{
				MachineLabelKey: machineName,
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
	if err := c.Create(context.Background(), node); err != nil {
		t.Fatalf("creating node: %v", err)
	}
	// Patch status (envtest requires a separate status update).
	if err := c.Status().Update(context.Background(), node); err != nil {
		t.Logf("patching node status (non-fatal): %v", err)
	}
	return node
}

// TestReconciler_NewMachine_Provisioning verifies that reconciling a new Machine CRD
// calls CreateInstance and moves the phase to Provisioning.
func TestReconciler_NewMachine_Provisioning(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, adapter := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-provisioning"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m1", "test-provisioning")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m1", Namespace: "test-provisioning"}}

	// First reconcile: should add finalizer, create instance, set Provisioning.
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseProvisioning {
		t.Errorf("phase: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhaseProvisioning)
	}
	if !containsString(updated.Finalizers, MachineFinalizer) {
		t.Error("finalizer not set on machine")
	}
	if updated.Status.InstanceId == "" {
		t.Error("instanceId should be set after CreateInstance")
	}

	// Verify the mock adapter has the instance.
	if adapter.InstanceCount() != 1 {
		t.Errorf("adapter instance count: got %d, want 1", adapter.InstanceCount())
	}
}

// TestReconciler_ProvisioningToReady verifies that once the node appears Ready,
// the machine transitions from Provisioning to Ready.
func TestReconciler_ProvisioningToReady(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, _ := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ready"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m2", "test-ready")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m2", Namespace: "test-ready"}}

	// First reconcile: Provisioning, instance created.
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseProvisioning {
		t.Fatalf("expected Provisioning, got %q", updated.Status.Phase)
	}

	// Simulate the k8s node joining.
	createReadyNode(t, k8sClient, "m2")

	// Second reconcile: node is Ready → transition to Ready.
	reconcileN(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReady {
		t.Errorf("phase: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhaseReady)
	}
}

// TestReconciler_DesiredStopped verifies that setting desiredPhase=Stopped on a Ready machine
// triggers StopInstance and moves the phase to Stopping then Stopped.
func TestReconciler_DesiredStopped(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, adapter := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-stopped"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m3", "test-stopped")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m3", Namespace: "test-stopped"}}

	// Provision to Ready.
	reconcileN(t, r, req, 1)
	createReadyNode(t, k8sClient, "m3")
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReady {
		t.Fatalf("expected Ready, got %q", updated.Status.Phase)
	}

	// Set desiredPhase=Stopped.
	patch := client.MergeFrom(updated.DeepCopy())
	updated.Spec.DesiredPhase = kyberv1.MachinePhaseStopped
	if err := k8sClient.Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching desiredPhase: %v", err)
	}

	// Reconcile: should call StopInstance and move to Stopping.
	reconcileN(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseStopping {
		t.Errorf("phase: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhaseStopping)
	}

	// Simulate VM becoming stopped in the adapter.
	instanceID := updated.Status.InstanceId
	if err := adapter.StopInstance(context.Background(), instanceID); err != nil {
		t.Fatalf("mock StopInstance: %v", err)
	}

	// Patch last transition back so we don't hit the timeout path.
	updated = getMachine(t, k8sClient, req.NamespacedName)
	patchBase := client.MergeFrom(updated.DeepCopy())
	now := metav1.NewTime(time.Now())
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patchBase); err != nil {
		t.Logf("patching last transition (non-fatal): %v", err)
	}

	// Reconcile: VM is now stopped → Stopped.
	reconcileN(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseStopped {
		t.Errorf("phase: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhaseStopped)
	}
}

// TestReconciler_Preemption verifies that when a spot instance is preempted,
// the controller detects it and provisions a replacement in the same zone.
func TestReconciler_Preemption(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, adapter := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-preemption"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m4", "test-preemption")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m4", Namespace: "test-preemption"}}

	// Provision to Ready.
	reconcileN(t, r, req, 1)
	createReadyNode(t, k8sClient, "m4")
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReady {
		t.Fatalf("expected Ready, got %q", updated.Status.Phase)
	}

	// Record the original instance ID.
	originalInstanceID := updated.Status.InstanceId

	// Simulate GCE preempting the instance.
	if err := adapter.SetPreempted(originalInstanceID); err != nil {
		t.Fatalf("SetPreempted: %v", err)
	}

	// Delete the node to simulate it disappearing.
	node := &corev1.Node{}
	nodeKey := types.NamespacedName{Name: "node-m4"}
	if err := k8sClient.Get(context.Background(), nodeKey, node); err == nil {
		if err := k8sClient.Delete(context.Background(), node); err != nil {
			t.Fatalf("deleting node: %v", err)
		}
	}

	// Reconcile: node gone + instance preempted → Preempted phase.
	reconcileN(t, r, req, 1)
	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhasePreempted {
		t.Errorf("phase after preemption: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhasePreempted)
	}

	// Reconcile: Preempted → triggers replacement → Replacing.
	reconcileN(t, r, req, 1)
	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReplacing {
		t.Errorf("phase after replacement start: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhaseReplacing)
	}

	// Verify a new instance was created (different from original).
	newInstanceID := updated.Status.InstanceId
	if newInstanceID == originalInstanceID {
		t.Error("replacement should have created a new instance ID")
	}
	if newInstanceID == "" {
		t.Error("replacement instanceId should not be empty")
	}

	// Verify replacement count was incremented.
	if updated.Status.ReplacementCount != 1 {
		t.Errorf("replacementCount: got %d, want 1", updated.Status.ReplacementCount)
	}

	// Simulate the replacement node joining.
	createReadyNode(t, k8sClient, "m4")

	// Reconcile: replacement node is Ready → Ready.
	reconcileN(t, r, req, 1)
	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReady {
		t.Errorf("phase after replacement complete: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhaseReady)
	}
}

// TestReconciler_Delete verifies that deleting a Machine CRD runs the finalizer,
// calls DeleteInstance, and removes the finalizer.
func TestReconciler_Delete(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, adapter := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-delete"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m5", "test-delete")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m5", Namespace: "test-delete"}}

	// Provision machine.
	reconcileN(t, r, req, 1)
	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.InstanceId == "" {
		t.Fatal("expected instanceId to be set after provisioning")
	}
	if adapter.InstanceCount() != 1 {
		t.Fatalf("expected 1 instance, got %d", adapter.InstanceCount())
	}

	// Delete the Machine CRD.
	if err := k8sClient.Delete(context.Background(), updated); err != nil {
		t.Fatalf("deleting machine: %v", err)
	}

	// Reconcile the deletion.
	reconcileN(t, r, req, 1)

	// The instance should have been deleted from the adapter.
	if adapter.InstanceCount() != 0 {
		t.Errorf("expected 0 instances after deletion, got %d", adapter.InstanceCount())
	}

	// The Machine CRD should be fully gone (finalizer removed).
	machine2 := &kyberv1.Machine{}
	err := k8sClient.Get(context.Background(), req.NamespacedName, machine2)
	if err == nil {
		t.Error("machine should be fully deleted after finalizer removal")
	}
	if !errors.IsNotFound(err) {
		t.Errorf("expected NotFound error, got: %v", err)
	}
}

// TestReconciler_Delete_NamespaceTerminating_SkipsExternalCleanup verifies
// that when the enclosing namespace is Terminating, the machine finalizer
// self-removes WITHOUT deprovisioning the external instance. The rationale:
// during a namespace teardown the controller itself is being torn down with
// it, so the finalizer has to shed load fast to avoid a deadlock. Operators
// accept that they own any external VM cleanup in the nuke-namespace case.
// See issue #64.
func TestReconciler_Delete_NamespaceTerminating_SkipsExternalCleanup(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, adapter := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-ns-terminating",
			Finalizers: []string{"kubernetes"},
		},
	}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m-ghost", "test-ns-terminating")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m-ghost", Namespace: "test-ns-terminating"}}

	// Provision.
	reconcileN(t, r, req, 1)
	if adapter.InstanceCount() != 1 {
		t.Fatalf("expected 1 instance after provision, got %d", adapter.InstanceCount())
	}

	// Tear namespace first (DeletionTimestamp set; held by dummy finalizer).
	if err := k8sClient.Delete(context.Background(), ns); err != nil {
		t.Fatalf("deleting namespace: %v", err)
	}
	updated := getMachine(t, k8sClient, req.NamespacedName)
	if err := k8sClient.Delete(context.Background(), updated); err != nil {
		t.Fatalf("deleting machine: %v", err)
	}

	// Reconcile: finalizer should self-remove without touching the instance.
	reconcileN(t, r, req, 1)

	// External instance must NOT have been deleted — the operator takes the
	// external cleanup burden in this escape-hatch path.
	if adapter.InstanceCount() != 1 {
		t.Errorf("external instance should remain untouched during namespace teardown; got count=%d", adapter.InstanceCount())
	}

	// Must emit a Warning event so operators can find orphaned VMs before
	// the namespace GC wipes the Machine CR.
	rec := r.Recorder.(*record.FakeRecorder)
	sawOrphanEvent := false
	drain := true
	for drain {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, "ExternalResourceOrphaned") && strings.Contains(ev, "gcloud compute instances delete") {
				sawOrphanEvent = true
			}
		default:
			drain = false
		}
	}
	if !sawOrphanEvent {
		t.Error("expected Warning event ExternalResourceOrphaned with gcloud cleanup hint; none found")
	}

	// Machine CRD should be gone (finalizer removed).
	machine2 := &kyberv1.Machine{}
	err := k8sClient.Get(context.Background(), req.NamespacedName, machine2)
	if err == nil {
		if containsString(machine2.Finalizers, MachineFinalizer) {
			t.Error("finalizer still present after namespace-terminating deletion reconcile")
		}
	} else if !errors.IsNotFound(err) {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReconciler_ProvisioningTimeout verifies that a machine stuck in Provisioning
// for more than 10 minutes moves to Failed.
func TestReconciler_ProvisioningTimeout(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, _ := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-timeout"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m6", "test-timeout")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m6", Namespace: "test-timeout"}}

	// First reconcile: Provisioning.
	reconcileN(t, r, req, 1)

	// Backdate LastTransition to simulate 11 minutes ago (past the 10 min timeout).
	updated := getMachine(t, k8sClient, req.NamespacedName)
	patchBase := client.MergeFrom(updated.DeepCopy())
	past := metav1.NewTime(time.Now().Add(-11 * time.Minute))
	updated.Status.LastTransition = &past
	if err := k8sClient.Status().Patch(context.Background(), updated, patchBase); err != nil {
		t.Fatalf("patching LastTransition: %v", err)
	}

	// Reconcile: timeout should trigger → Failed.
	reconcileN(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseFailed {
		t.Errorf("phase: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhaseFailed)
	}
}

// reconcileIgnoringErrors calls Reconcile n times, ignoring non-fatal action errors.
// Action errors cause the reconciler to requeue — the test drives reconciles explicitly,
// so we don't treat them as fatal test failures. This differs from reconcileN which
// fatals on any error; use this helper when the reconciler is expected to log-and-requeue.
func reconcileIgnoringErrors(t *testing.T, r *MachineReconciler, req ctrl.Request, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		// The reconciler only propagates errors from k8s API failures.
		// Action failures are returned as (RequeueAfter, nil) — so Reconcile itself
		// returns nil. We log any unexpected errors for debugging.
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Logf("reconcile %d/%d: unexpected error (continuing): %v", i+1, n, err)
		}
	}
}

// TestReconciler_ReplacementRetryExhaustion verifies that when CreateInstance fails
// repeatedly, ReplacementCount increments each time (even on failure), and the machine
// transitions to Failed once MaxReplacementRetries is reached.
//
// This tests the fix for the dead-code bug: previously, ReplacementCount was only
// incremented on success, so the retry limit was never reached and the machine looped
// in Replacing/Preempted forever. Now the count is incremented before the create attempt.
func TestReconciler_ReplacementRetryExhaustion(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, adapter := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-retry-exhaust"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m7", "test-retry-exhaust")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m7", Namespace: "test-retry-exhaust"}}

	// Provision to Ready.
	reconcileN(t, r, req, 1)
	createReadyNode(t, k8sClient, "m7")
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReady {
		t.Fatalf("expected Ready, got %q", updated.Status.Phase)
	}

	// Simulate preemption.
	originalInstanceID := updated.Status.InstanceId
	if err := adapter.SetPreempted(originalInstanceID); err != nil {
		t.Fatalf("SetPreempted: %v", err)
	}
	node := &corev1.Node{}
	nodeKey := types.NamespacedName{Name: "node-m7"}
	if err := k8sClient.Get(context.Background(), nodeKey, node); err == nil {
		if err := k8sClient.Delete(context.Background(), node); err != nil {
			t.Fatalf("deleting node: %v", err)
		}
	}

	// Reconcile: node gone + preempted → Preempted.
	reconcileN(t, r, req, 1)
	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhasePreempted {
		t.Fatalf("expected Preempted, got %q", updated.Status.Phase)
	}

	// Configure CreateInstance to always fail from this point on.
	createErr := fmt.Errorf("quota exceeded: no spot capacity")
	adapter.SetCreateError(createErr)

	// Reconcile MaxReplacementRetries times — each triggers a failed CreateInstance
	// which increments ReplacementCount but keeps the machine in Preempted phase.
	// reconcileIgnoringErrors is used because action errors are non-fatal (requeue).
	reconcileIgnoringErrors(t, r, req, MaxReplacementRetries)

	// One more reconcile: ReplacementCount==MaxReplacementRetries →
	// EventReplacementRetryLimitReached → ActionMarkReplacementFailed → Failed.
	reconcileIgnoringErrors(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)

	if updated.Status.Phase != kyberv1.MachinePhaseFailed {
		t.Errorf("phase: got %q, want %q", updated.Status.Phase, kyberv1.MachinePhaseFailed)
	}
	if updated.Status.ReplacementCount != int32(MaxReplacementRetries) {
		t.Errorf("ReplacementCount: got %d, want %d", updated.Status.ReplacementCount, MaxReplacementRetries)
	}

	// After reaching Failed, clear the error and confirm no further CreateInstance calls happen.
	adapter.SetCreateError(nil)
	instanceCountBefore := adapter.InstanceCount()
	reconcileIgnoringErrors(t, r, req, 2) // phase=Failed, desired not set — should stay stable
	if adapter.InstanceCount() != instanceCountBefore {
		t.Errorf("unexpected CreateInstance calls after Failed: count changed from %d to %d",
			instanceCountBefore, adapter.InstanceCount())
	}
}

// TestReconciler_StoppingWaitsForAgentDrain verifies that a machine in Stopping phase
// does not transition to Stopped until all agent pods have drained.
//
// This tests fix #2: classifyStopping now checks agent count before emitting
// EventAllAgentsStoppedVMStopped. A machine with live agents should keep requeueing
// rather than immediately stopping the VM.
func TestReconciler_StoppingWaitsForAgentDrain(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, adapter := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-drain"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m8", "test-drain")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m8", Namespace: "test-drain"}}

	// Provision to Ready.
	reconcileN(t, r, req, 1)
	createReadyNode(t, k8sClient, "m8")
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReady {
		t.Fatalf("expected Ready, got %q", updated.Status.Phase)
	}

	// Inject an AgentCountFn that reports 1 agent running.
	// Using atomic so we can flip it from the test without data races.
	var agentCount int32 = 1
	r.AgentCountFn = func(_ context.Context, _ *kyberv1.Machine) (int, error) {
		return int(atomic.LoadInt32(&agentCount)), nil
	}

	// Set desiredPhase=Stopped.
	patch := client.MergeFrom(updated.DeepCopy())
	updated.Spec.DesiredPhase = kyberv1.MachinePhaseStopped
	if err := k8sClient.Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching desiredPhase: %v", err)
	}

	// Reconcile: Ready + desiredStopped → Stopping (calls StopVM).
	reconcileN(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseStopping {
		t.Fatalf("expected Stopping, got %q", updated.Status.Phase)
	}

	// Simulate VM becoming stopped in the adapter.
	instanceID := updated.Status.InstanceId
	if err := adapter.StopInstance(context.Background(), instanceID); err != nil {
		t.Fatalf("mock StopInstance: %v", err)
	}

	// Reconcile while agent is still running: should NOT transition to Stopped.
	reconcileIgnoringErrors(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseStopping {
		t.Errorf("phase: got %q, want Stopping — machine should not stop while agents are running", updated.Status.Phase)
	}

	// Simulate agent drain completing.
	atomic.StoreInt32(&agentCount, 0)

	// Patch LastTransition to now so we don't hit the stopping timeout.
	updated = getMachine(t, k8sClient, req.NamespacedName)
	patchBase := client.MergeFrom(updated.DeepCopy())
	now := metav1.NewTime(time.Now())
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patchBase); err != nil {
		t.Logf("patching LastTransition (non-fatal): %v", err)
	}

	// Reconcile: agents drained + VM stopped → Stopped.
	reconcileN(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseStopped {
		t.Errorf("phase: got %q, want Stopped", updated.Status.Phase)
	}
}

// TestHandlePreemptionNotice_AnnotatesAgents verifies that HandlePreemptionNotice sets
// the "kyber.dev/preemption-notice" annotation on Running and Starting agents assigned
// to the preempted machine, and leaves other agents untouched.
func TestHandlePreemptionNotice_AnnotatesAgents(t *testing.T) {
	scheme := buildTestScheme()
	const ns = "kyber-system"

	// Machine being preempted.
	machine := newTestMachine("preempted-machine", ns)

	// Running agent on the preempted machine — should be annotated.
	runningAgent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-running", Namespace: ns},
		Spec: kyberv1.AgentSpec{
			Machine:  "preempted-machine",
			Runtime:  "claude-code",
			Model:    "claude-sonnet-4",
			Scaling:  kyberv1.AgentScalingWarm,
			Secrets:  kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			Resources: kyberv1.AgentResources{},
		},
		Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning},
	}

	// Starting agent on the preempted machine — should be annotated.
	startingAgent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-starting", Namespace: ns},
		Spec: kyberv1.AgentSpec{
			Machine:  "preempted-machine",
			Runtime:  "claude-code",
			Model:    "claude-sonnet-4",
			Scaling:  kyberv1.AgentScalingWarm,
			Secrets:  kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			Resources: kyberv1.AgentResources{},
		},
		Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseStarting},
	}

	// Stopped agent on the preempted machine — should NOT be annotated.
	stoppedAgent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-stopped", Namespace: ns},
		Spec: kyberv1.AgentSpec{
			Machine:  "preempted-machine",
			Runtime:  "claude-code",
			Model:    "claude-sonnet-4",
			Scaling:  kyberv1.AgentScalingWarm,
			Secrets:  kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			Resources: kyberv1.AgentResources{},
		},
		Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseStopped},
	}

	// Running agent on a different machine — should NOT be annotated.
	otherMachineAgent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-other-machine", Namespace: ns},
		Spec: kyberv1.AgentSpec{
			Machine:  "other-machine",
			Runtime:  "claude-code",
			Model:    "claude-sonnet-4",
			Scaling:  kyberv1.AgentScalingWarm,
			Secrets:  kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeOAuth},
			Resources: kyberv1.AgentResources{},
		},
		Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, runningAgent, startingAgent, stoppedAgent, otherMachineAgent).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()

	r := &MachineReconciler{
		Client:    fakeClient,
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(10),
		Namespace: ns,
	}

	ctx := context.Background()
	r.HandlePreemptionNotice(ctx, "preempted-machine", "kyber-testvm-abc123")

	// Verify running agent was annotated.
	var got kyberv1.Agent
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "agent-running", Namespace: ns}, &got); err != nil {
		t.Fatalf("getting running agent: %v", err)
	}
	if _, ok := got.Annotations["kyber.dev/preemption-notice"]; !ok {
		t.Error("running agent: expected kyber.dev/preemption-notice annotation, not found")
	}

	// Verify starting agent was annotated.
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "agent-starting", Namespace: ns}, &got); err != nil {
		t.Fatalf("getting starting agent: %v", err)
	}
	if _, ok := got.Annotations["kyber.dev/preemption-notice"]; !ok {
		t.Error("starting agent: expected kyber.dev/preemption-notice annotation, not found")
	}

	// Verify stopped agent was NOT annotated.
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "agent-stopped", Namespace: ns}, &got); err != nil {
		t.Fatalf("getting stopped agent: %v", err)
	}
	if _, ok := got.Annotations["kyber.dev/preemption-notice"]; ok {
		t.Error("stopped agent: unexpected kyber.dev/preemption-notice annotation")
	}

	// Verify other-machine agent was NOT annotated.
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "agent-other-machine", Namespace: ns}, &got); err != nil {
		t.Fatalf("getting other-machine agent: %v", err)
	}
	if _, ok := got.Annotations["kyber.dev/preemption-notice"]; ok {
		t.Error("other-machine agent: unexpected kyber.dev/preemption-notice annotation")
	}
}

// createReadyNodeWithAllocatable creates a k8s Node with ready condition and specific
// allocatable resource quantities. Used to test that the reconciler plumbs
// Node.status.allocatable into Machine.status.allocatable.
func createReadyNodeWithAllocatable(t *testing.T, c client.Client, machineName string, cpu, memory resource.Quantity) *corev1.Node {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-" + machineName,
			Labels: map[string]string{
				MachineLabelKey: machineName,
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
				},
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    cpu,
				corev1.ResourceMemory: memory,
			},
		},
	}
	if err := c.Create(context.Background(), node); err != nil {
		t.Fatalf("creating node with allocatable: %v", err)
	}
	if err := c.Status().Update(context.Background(), node); err != nil {
		t.Logf("patching node status (non-fatal): %v", err)
	}
	return node
}

// TestReconciler_AllocatablePopulatedFromNode verifies that when a Machine transitions to
// Ready and its backing node has allocatable resources, Machine.status.observedCapacity is
// populated from Node.status.allocatable.
func TestReconciler_AllocatablePopulatedFromNode(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, _ := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-allocatable"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m-alloc", "test-allocatable")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m-alloc", Namespace: "test-allocatable"}}

	// First reconcile: Provisioning, instance created.
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseProvisioning {
		t.Fatalf("expected Provisioning, got %q", updated.Status.Phase)
	}

	// Create a node with specific allocatable values (simulating what k3s reports after
	// reserving overhead for system components).
	wantCPU := resource.MustParse("1750m")
	wantMem := resource.MustParse("7300Mi")
	createReadyNodeWithAllocatable(t, k8sClient, "m-alloc", wantCPU, wantMem)

	// Second reconcile: node is Ready → transition to Ready, updateNodeInfo called.
	reconcileN(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReady {
		t.Errorf("phase: got %q, want Ready", updated.Status.Phase)
	}

	if updated.Status.ObservedCapacity == nil {
		t.Fatal("status.observedCapacity should be set after transitioning to Ready with a node")
	}
	if updated.Status.ObservedCapacity.CPU.Cmp(wantCPU) != 0 {
		t.Errorf("observedCapacity.cpu: got %s, want %s",
			updated.Status.ObservedCapacity.CPU.String(), wantCPU.String())
	}
	if updated.Status.ObservedCapacity.Memory.Cmp(wantMem) != 0 {
		t.Errorf("observedCapacity.memory: got %s, want %s",
			updated.Status.ObservedCapacity.Memory.String(), wantMem.String())
	}
}

// TestReconciler_DoublePreemption verifies that when a replacement VM is itself
// preempted (TERMINATED), the reconciler fires EventReplacementFailed and retries
// instead of requeuing forever.
func TestReconciler_DoublePreemption(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, adapter := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-double-preempt"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := newTestMachine("m-double", "test-double-preempt")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "m-double", Namespace: "test-double-preempt"}}

	// Provision to Ready.
	reconcileN(t, r, req, 1)
	createReadyNode(t, k8sClient, "m-double")
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReady {
		t.Fatalf("expected Ready, got %q", updated.Status.Phase)
	}
	originalInstanceID := updated.Status.InstanceId

	// Simulate first preemption.
	if err := adapter.SetPreempted(originalInstanceID); err != nil {
		t.Fatalf("SetPreempted: %v", err)
	}
	node := &corev1.Node{}
	nodeKey := types.NamespacedName{Name: "node-m-double"}
	if err := k8sClient.Get(context.Background(), nodeKey, node); err == nil {
		if err := k8sClient.Delete(context.Background(), node); err != nil {
			t.Fatalf("deleting node: %v", err)
		}
	}

	// Reconcile: node gone + preempted → Preempted → Replacing (creates replacement).
	reconcileN(t, r, req, 1) // → Preempted
	reconcileN(t, r, req, 1) // → Replacing (creates replacement instance)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseReplacing {
		t.Fatalf("expected Replacing, got %q", updated.Status.Phase)
	}
	replacementInstanceID := updated.Status.InstanceId
	if replacementInstanceID == originalInstanceID {
		t.Fatal("replacement should have a different instance ID")
	}

	// Simulate the replacement VM also getting preempted (double preemption).
	if err := adapter.SetPreempted(replacementInstanceID); err != nil {
		t.Fatalf("SetPreempted on replacement: %v", err)
	}

	// Reconcile: replacement is TERMINATED → EventReplacementFailed → ActionRetryReplacement.
	// This should NOT deadlock — it should fire the retry and stay in Replacing with a
	// new replacement attempt (or transition through Replacing again).
	reconcileIgnoringErrors(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)
	// The machine should still be in Replacing (retry creates a new instance)
	// and the replacement count should have incremented.
	if updated.Status.Phase != kyberv1.MachinePhaseReplacing {
		t.Errorf("phase after double preemption: got %q, want Replacing", updated.Status.Phase)
	}
	if updated.Status.ReplacementCount < 2 {
		t.Errorf("ReplacementCount should be >= 2 after double preemption, got %d", updated.Status.ReplacementCount)
	}
	// The instance ID should have changed again (third instance created).
	if updated.Status.InstanceId == replacementInstanceID {
		t.Error("expected a new instance ID after retry, got the same terminated one")
	}
}

// TestReconcile_MockProvider_SetsReadyWithNodeName verifies that a Machine with
// provider=mock skips the compute adapter and sets Phase=Ready + NodeName from
// the first Ready k8s node found.
func TestReconcile_MockProvider_SetsReadyWithNodeName(t *testing.T) {
	ctx := context.Background()
	nodeName := "demo-node"

	scheme := buildTestScheme()

	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine, node).
		WithStatusSubresource(&kyberv1.Machine{}).
		Build()

	r := &MachineReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		AgentCountFn: func(_ context.Context, _ *kyberv1.Machine) (int, error) {
			return 0, nil
		},
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "kyber-system", Name: "local"},
	}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	var got kyberv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kyber-system", Name: "local"}, &got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.Status.Phase != kyberv1.MachinePhaseReady {
		t.Errorf("phase = %s, want Ready", got.Status.Phase)
	}
	if got.Status.NodeName != nodeName {
		t.Errorf("nodeName = %s, want %s", got.Status.NodeName, nodeName)
	}
}

// TestReconcile_MockProvider_PopulatesCapacityFields covers kyber#240 — mock
// reconcile now writes ObservedCapacity / AssignableCapacity / AvailableCapacity
// directly from the chosen node (the GCE updateNodeInfo path can't help —
// getNodeForMachine filters by a label only set by the GCE startup script).
// Without this fix, MachineDetail rendered all-zero bars on standalone.
func TestReconcile_MockProvider_PopulatesCapacityFields(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme()

	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:              resource.MustParse("8"),
				Memory:           resource.MustParse("32Gi"),
				EphemeralStorage: resource.MustParse("400Gi"),
			},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-node"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("8"),
				corev1.ResourceMemory:           resource.MustParse("32Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("400Gi"),
			},
		},
	}
	// One existing agent on this machine consuming some capacity — verifies
	// AvailableCapacity = AssignableCapacity - sum(agent requests).
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine: "local",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("8Gi"),
				Disk:   resource.MustParse("100Gi"),
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine, node, agent).
		WithStatusSubresource(&kyberv1.Machine{}).
		Build()

	r := &MachineReconciler{
		Client:              c,
		Scheme:              scheme,
		Recorder:            record.NewFakeRecorder(10),
		PlatformReservation: kyberv1.MachineCapacity{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), EphemeralStorage: resource.MustParse("10Gi")},
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "kyber-system", Name: "local"},
	}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	var got kyberv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kyber-system", Name: "local"}, &got); err != nil {
		t.Fatalf("get machine: %v", err)
	}

	if got.Status.ObservedCapacity == nil {
		t.Fatal("ObservedCapacity nil; mock reconcile didn't populate")
	}
	if got.Status.ObservedCapacity.CPU.Cmp(resource.MustParse("8")) != 0 {
		t.Errorf("ObservedCapacity.CPU: got %s, want 8", got.Status.ObservedCapacity.CPU.String())
	}
	if got.Status.ObservedCapacity.EphemeralStorage.Cmp(resource.MustParse("400Gi")) != 0 {
		t.Errorf("ObservedCapacity.EphemeralStorage: got %s, want 400Gi", got.Status.ObservedCapacity.EphemeralStorage.String())
	}

	if got.Status.AssignableCapacity == nil {
		t.Fatal("AssignableCapacity nil")
	}
	// 8 - 1 = 7
	if got.Status.AssignableCapacity.CPU.Cmp(resource.MustParse("7")) != 0 {
		t.Errorf("AssignableCapacity.CPU: got %s, want 7", got.Status.AssignableCapacity.CPU.String())
	}
	// 400Gi - 10Gi = 390Gi
	if got.Status.AssignableCapacity.EphemeralStorage.Cmp(resource.MustParse("390Gi")) != 0 {
		t.Errorf("AssignableCapacity.EphemeralStorage: got %s, want 390Gi", got.Status.AssignableCapacity.EphemeralStorage.String())
	}

	if got.Status.AvailableCapacity == nil {
		t.Fatal("AvailableCapacity nil")
	}
	// 7 - 2 = 5
	if got.Status.AvailableCapacity.CPU.Cmp(resource.MustParse("5")) != 0 {
		t.Errorf("AvailableCapacity.CPU: got %s, want 5", got.Status.AvailableCapacity.CPU.String())
	}
	// 390Gi - 100Gi = 290Gi
	if got.Status.AvailableCapacity.EphemeralStorage.Cmp(resource.MustParse("290Gi")) != 0 {
		t.Errorf("AvailableCapacity.EphemeralStorage: got %s, want 290Gi", got.Status.AvailableCapacity.EphemeralStorage.String())
	}

	if got.Status.AgentCount != 1 {
		t.Errorf("AgentCount: got %d, want 1", got.Status.AgentCount)
	}
}

// TestReconcile_MockProvider_NoReadyNode_Requeues verifies that when no Ready node
// exists, the reconciler returns RequeueAfter > 0 and leaves the Machine phase unchanged.
func TestReconcile_MockProvider_NoReadyNode_Requeues(t *testing.T) {
	ctx := context.Background()

	scheme := buildTestScheme()

	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	// Node exists but its Ready condition is False.
	notReadyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "not-ready-node"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine, notReadyNode).
		WithStatusSubresource(&kyberv1.Machine{}).
		Build()

	r := &MachineReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		AgentCountFn: func(_ context.Context, _ *kyberv1.Machine) (int, error) {
			return 0, nil
		},
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "kyber-system", Name: "local"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 when no Ready node exists")
	}

	var got kyberv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kyber-system", Name: "local"}, &got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.Status.Phase != "" {
		t.Errorf("phase = %s, want empty (no transition when no Ready node)", got.Status.Phase)
	}
}

// TestSelectMockNode covers the two-tier node selection a provider=mock
// Machine uses: an explicit kyber.io/machine=<name> label wins, and the
// first Ready node is the fallback for unlabelled standalone installs.
//
// The label tier exists so multi-node clusters (GKE/EKS) can bind a Machine
// to a specific node pool. status.nodeName is what BuildPodSpec hard-pins
// agent pods to, so an unstable pick would strand or migrate agent pods.
func TestSelectMockNode(t *testing.T) {
	ready := func(name string, labels map[string]string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				},
			},
		}
	}
	notReady := func(name string, labels map[string]string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
				},
			},
		}
	}

	tests := []struct {
		name     string
		nodes    []client.Object
		wantNode string
	}{
		{
			name:     "unlabelled single node falls back to first Ready",
			nodes:    []client.Object{ready("solo", nil)},
			wantNode: "solo",
		},
		{
			name: "labelled node wins over unlabelled Ready nodes",
			nodes: []client.Object{
				ready("platform-node", nil),
				ready("agent-node", map[string]string{MachineLabelKey: "local"}),
			},
			wantNode: "agent-node",
		},
		{
			name: "label for a different machine is ignored",
			nodes: []client.Object{
				ready("other-machine-node", map[string]string{MachineLabelKey: "somebody-else"}),
			},
			// No node claims "local", so the fallback picks the only Ready node.
			wantNode: "other-machine-node",
		},
		{
			name:     "labelled but NotReady does not fall through to another node",
			nodes:    []client.Object{notReady("agent-node", map[string]string{MachineLabelKey: "local"}), ready("platform-node", nil)},
			wantNode: "",
		},
		{
			name:     "no Ready node at all",
			nodes:    []client.Object{notReady("down", nil)},
			wantNode: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := buildTestScheme()
			machine := &kyberv1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
				Spec:       kyberv1.MachineSpec{Provider: kyberv1.MachineProviderMock},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(append([]client.Object{machine}, tt.nodes...)...).
				WithStatusSubresource(&kyberv1.Machine{}).
				Build()

			r := &MachineReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

			got, err := r.selectMockNode(ctx, machine)
			if err != nil {
				t.Fatalf("selectMockNode: %v", err)
			}
			if tt.wantNode == "" {
				if got != nil {
					t.Fatalf("node = %s, want nil", got.Name)
				}
				return
			}
			if got == nil {
				t.Fatalf("node = nil, want %s", tt.wantNode)
			}
			if got.Name != tt.wantNode {
				t.Errorf("node = %s, want %s", got.Name, tt.wantNode)
			}
		})
	}
}

// TestReconcile_MockProvider_PrefersLabelledNode is the end-to-end version of
// the label tier: status.nodeName must land on the labelled node even when
// another Ready node exists, because that field is what agent pods pin to.
func TestReconcile_MockProvider_PrefersLabelledNode(t *testing.T) {
	ctx := context.Background()
	scheme := buildTestScheme()

	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "kyber-system"},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderMock,
			Capacity: kyberv1.MachineCapacity{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("4Gi"),
			},
		},
	}
	mkNode := func(name string, labels map[string]string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				},
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		}
	}
	// "a-platform" sorts before "z-agents" — without the label tier, a
	// name-ordered List would pick the platform node.
	platformNode := mkNode("a-platform", nil)
	agentNode := mkNode("z-agents", map[string]string{MachineLabelKey: "local"})

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine, platformNode, agentNode).
		WithStatusSubresource(&kyberv1.Machine{}).
		Build()

	r := &MachineReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		AgentCountFn: func(_ context.Context, _ *kyberv1.Machine) (int, error) {
			return 0, nil
		},
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "kyber-system", Name: "local"},
	}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	var got kyberv1.Machine
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kyber-system", Name: "local"}, &got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.Status.NodeName != "z-agents" {
		t.Errorf("nodeName = %s, want z-agents (the labelled node)", got.Status.NodeName)
	}
	if got.Status.Phase != kyberv1.MachinePhaseReady {
		t.Errorf("phase = %s, want Ready", got.Status.Phase)
	}
}

// TestMachineReconciler_PopulatesAllCapacityFields verifies that on a normal
// reconcile against a Ready node with two agents bound, the controller writes
// all three new status capacity fields with the expected values.
//
// Scenario:
//   - PlatformReservation: 1 CPU / 1Gi
//   - Node allocatable: 4 CPU / 8Gi
//   - Two agents on this machine: alice (1 / 2Gi), bob (500m / 1Gi)
//
// Expected:
//   - ObservedCapacity   = 4 / 8Gi
//   - AssignableCapacity = 3 / 7Gi
//   - AvailableCapacity  = 1500m / 4Gi
//
// Regression for #140.
func TestMachineReconciler_PopulatesAllCapacityFields(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, _ := newReconciler(k8sClient, scheme)
	r.PlatformReservation = kyberv1.MachineCapacity{
		CPU:    resource.MustParse("1"),
		Memory: resource.MustParse("1Gi"),
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cap-fields"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	// Machine pointing at the node (create before the node so we can see the phase transition).
	machine := newTestMachine("razer", "test-cap-fields")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "razer", Namespace: "test-cap-fields"}}

	// First reconcile: CRDCreated → Provisioning (creates mock instance).
	reconcileN(t, r, req, 1)

	updated := getMachine(t, k8sClient, req.NamespacedName)
	if updated.Status.Phase != kyberv1.MachinePhaseProvisioning {
		t.Fatalf("expected Provisioning after first reconcile, got %q", updated.Status.Phase)
	}

	// Node with allocatable 4 / 8Gi labeled for our machine.
	createReadyNodeWithAllocatable(t, k8sClient, "razer", resource.MustParse("4"), resource.MustParse("8Gi"))

	// Two agents on this machine.
	for _, spec := range []struct{ name, cpu, mem string }{
		{"alice", "1", "2Gi"},
		{"bob", "500m", "1Gi"},
	} {
		a := &kyberv1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: spec.name, Namespace: "test-cap-fields"},
			Spec: kyberv1.AgentSpec{
				Machine: "razer",
				Runtime: "stub",
				Model:   "claude-sonnet-4",
				Scaling: kyberv1.AgentScalingWarm,
				Secrets: kyberv1.AgentSecrets{AuthType: kyberv1.AgentAuthTypeAPIKey},
				Resources: kyberv1.AgentResources{
					CPU:    resource.MustParse(spec.cpu),
					Memory: resource.MustParse(spec.mem),
					Disk:   resource.MustParse("1Gi"),
				},
			},
		}
		if err := k8sClient.Create(context.Background(), a); err != nil {
			t.Fatalf("creating agent %s: %v", spec.name, err)
		}
	}

	// Second reconcile: node is Ready → transition to Ready, updateNodeInfo called.
	reconcileN(t, r, req, 1)

	updated = getMachine(t, k8sClient, req.NamespacedName)

	if updated.Status.ObservedCapacity == nil {
		t.Fatal("ObservedCapacity is nil")
	}
	wantObservedCPU := resource.MustParse("4")
	wantObservedMem := resource.MustParse("8Gi")
	if updated.Status.ObservedCapacity.CPU.Cmp(wantObservedCPU) != 0 {
		t.Errorf("ObservedCapacity.CPU: got %q, want %q", updated.Status.ObservedCapacity.CPU.String(), wantObservedCPU.String())
	}
	if updated.Status.ObservedCapacity.Memory.Cmp(wantObservedMem) != 0 {
		t.Errorf("ObservedCapacity.Memory: got %q, want %q", updated.Status.ObservedCapacity.Memory.String(), wantObservedMem.String())
	}

	if updated.Status.AssignableCapacity == nil {
		t.Fatal("AssignableCapacity is nil")
	}
	wantAssignableCPU := resource.MustParse("3")
	wantAssignableMem := resource.MustParse("7Gi")
	if updated.Status.AssignableCapacity.CPU.Cmp(wantAssignableCPU) != 0 {
		t.Errorf("AssignableCapacity.CPU: got %q, want %q", updated.Status.AssignableCapacity.CPU.String(), wantAssignableCPU.String())
	}
	if updated.Status.AssignableCapacity.Memory.Cmp(wantAssignableMem) != 0 {
		t.Errorf("AssignableCapacity.Memory: got %q, want %q", updated.Status.AssignableCapacity.Memory.String(), wantAssignableMem.String())
	}

	if updated.Status.AvailableCapacity == nil {
		t.Fatal("AvailableCapacity is nil")
	}
	wantAvailableCPU := resource.MustParse("1500m")
	wantAvailableMem := resource.MustParse("4Gi")
	if updated.Status.AvailableCapacity.CPU.Cmp(wantAvailableCPU) != 0 {
		t.Errorf("AvailableCapacity.CPU: got %q, want %q", updated.Status.AvailableCapacity.CPU.String(), wantAvailableCPU.String())
	}
	if updated.Status.AvailableCapacity.Memory.Cmp(wantAvailableMem) != 0 {
		t.Errorf("AvailableCapacity.Memory: got %q, want %q", updated.Status.AvailableCapacity.Memory.String(), wantAvailableMem.String())
	}
}

// TestMachineReconciler_ClearsCapacityFieldsWhenNodeGone verifies that when a
// machine has its capacity status populated, then the backing node is deleted
// (preemption / manual removal), the next call to updateNodeInfo clears all
// three capacity fields back to nil. Without this guard, a stale
// AvailableCapacity could survive the node-gone transition and the API
// consumer would admit an agent based on capacity that no longer exists.
//
// Implementation note: the state machine does not call updateNodeInfo during the
// Ready→Failed transition that fires when a node disappears (that path executes
// ActionMarkFailed, not ActionUpdateStatus). The node==nil clearing branch in
// updateNodeInfo is a defensive guard for the race where a node disappears
// between the classify step and the ActionUpdateStatus execute step, and for
// future callers. This test exercises the branch directly (valid because we are
// in package machine) to provide a regression anchor.
//
// Regression for #140 (the cleared-on-nil branch of updateNodeInfo).
func TestMachineReconciler_ClearsCapacityFieldsWhenNodeGone(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r, _ := newReconciler(k8sClient, scheme)
	r.PlatformReservation = kyberv1.MachineCapacity{
		CPU:    resource.MustParse("1"),
		Memory: resource.MustParse("1Gi"),
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cap-clear"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	// Stand up a Ready node + machine so all three fields populate.
	// The node must be created before reconcile so classifyProvisioning sees it.
	createReadyNodeWithAllocatable(t, k8sClient, "razer-clear",
		resource.MustParse("4"), resource.MustParse("8Gi"))

	machine := newTestMachine("razer-clear", "test-cap-clear")
	if err := k8sClient.Create(context.Background(), machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "razer-clear", Namespace: "test-cap-clear"}}

	// Two reconciles: CRDCreated→Provisioning, then Provisioning→Ready (node present).
	reconcileN(t, r, req, 2)

	// Sanity check: capacity is populated before we delete the node.
	pre := &kyberv1.Machine{}
	if err := k8sClient.Get(context.Background(), req.NamespacedName, pre); err != nil {
		t.Fatalf("getting machine pre-delete: %v", err)
	}
	if pre.Status.ObservedCapacity == nil || pre.Status.AssignableCapacity == nil || pre.Status.AvailableCapacity == nil {
		t.Fatalf("capacity not populated before node-gone repro: Observed=%v Assignable=%v Available=%v",
			pre.Status.ObservedCapacity, pre.Status.AssignableCapacity, pre.Status.AvailableCapacity)
	}

	// Delete the node. updateNodeInfo's node==nil branch fires when called directly.
	// We call updateNodeInfo directly here because the state machine's Ready→Failed
	// transition (ActionMarkFailed) does not invoke updateNodeInfo — the nil-clearing
	// is a defensive guard for concurrent disappearance races and future call sites.
	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(context.Background(), nodeList, client.MatchingLabels{MachineLabelKey: "razer-clear"}); err != nil {
		t.Fatalf("listing nodes: %v", err)
	}
	if len(nodeList.Items) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodeList.Items))
	}
	if err := k8sClient.Delete(context.Background(), &nodeList.Items[0]); err != nil {
		t.Fatalf("deleting node: %v", err)
	}

	// Re-fetch machine so updateNodeInfo has a fresh copy to patch.
	live := &kyberv1.Machine{}
	if err := k8sClient.Get(context.Background(), req.NamespacedName, live); err != nil {
		t.Fatalf("re-fetching machine: %v", err)
	}

	// Call updateNodeInfo directly — this is the function under test for the nil branch.
	if err := r.updateNodeInfo(context.Background(), live); err != nil {
		t.Fatalf("updateNodeInfo with deleted node: %v", err)
	}

	post := &kyberv1.Machine{}
	if err := k8sClient.Get(context.Background(), req.NamespacedName, post); err != nil {
		t.Fatalf("getting machine post-delete: %v", err)
	}
	if post.Status.ObservedCapacity != nil {
		t.Errorf("ObservedCapacity should be nil after node deletion, got %+v", post.Status.ObservedCapacity)
	}
	if post.Status.AssignableCapacity != nil {
		t.Errorf("AssignableCapacity should be nil after node deletion, got %+v", post.Status.AssignableCapacity)
	}
	if post.Status.AvailableCapacity != nil {
		t.Errorf("AvailableCapacity should be nil after node deletion, got %+v", post.Status.AvailableCapacity)
	}
}
