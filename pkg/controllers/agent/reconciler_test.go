package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/githubapp"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
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

// newReconciler builds a test AgentReconciler wired with the stub adapter and a MemoryStore.
func newReconciler(k8sClient client.Client, scheme *runtime.Scheme) *AgentReconciler {
	return newReconcilerWithStore(k8sClient, scheme, briefstore.NewMemoryStore())
}

// newReconcilerWithStore builds a test AgentReconciler with a caller-provided BriefStore.
// Used in tests that need to inspect what was written to the store.
func newReconcilerWithStore(k8sClient client.Client, scheme *runtime.Scheme, store briefstore.BriefStore) *AgentReconciler {
	liveness := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"pgrep", "-f", "claude"}},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       30,
		FailureThreshold:    3,
	}
	adapter := pkgruntimes.NewStubAdapter(
		"ghcr.io/matty-v/agent-claude-code:latest",
		[]string{"/usr/local/bin/start-claude.sh"},
		[]corev1.EnvVar{{Name: "CLAUDE_MODEL", Value: "claude-sonnet-4"}},
		nil, // no secret mounts in unit tests
		liveness, nil,
		30,
		"/persist/session-brief.json",
		"/persist/session-state.json",
		"CLAUDE_MODEL",
	)
	return &AgentReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(100),
		AdapterRegistry: map[string]pkgruntimes.Adapter{
			"stub":        adapter,
			"claude-code": adapter,
		},
		BriefStore: store,
	}
}

// buildTestScheme builds the scheme used in reconciler construction.
func buildTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = kyberv1.AddToScheme(s)
	return s
}

// newTestAgent returns a minimal Agent in the given namespace.
func newTestAgent(name, namespace string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: kyberv1.AgentSpec{
			Machine: "node-01",
			Runtime: "stub",
			Model:   "claude-sonnet-4",
			Resources: kyberv1.AgentResources{
				CPU:    resource.MustParse("100m"),
				Memory: resource.MustParse("256Mi"),
				Disk:   resource.MustParse("1Gi"),
			},
			Secrets: kyberv1.AgentSecrets{
				AuthType: kyberv1.AgentAuthTypeOAuth,
			},
		},
	}
}

// reconcileN calls Reconcile n times, stopping early on error.
func reconcileN(t *testing.T, r *AgentReconciler, req ctrl.Request, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d/%d failed: %v", i+1, n, err)
		}
	}
}

// getAgent fetches the Agent from the API server.
func getAgent(t *testing.T, c client.Client, key types.NamespacedName) *kyberv1.Agent {
	t.Helper()
	agent := &kyberv1.Agent{}
	if err := c.Get(context.Background(), key, agent); err != nil {
		t.Fatalf("getting agent: %v", err)
	}
	return agent
}

// TestReconciler_StampsObservedGenerationOnPodCreate verifies kyber#157 PR-A:
// after a successful pod create, the reconciler patches
// Agent.status.observedGeneration to match metadata.generation. This is what
// the PWA's "restart required" badge keys off of (it derives dirty as
// generation > observedGeneration).
func TestReconciler_StampsObservedGenerationOnPodCreate(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-stamping"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-stamping")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-stamping"}}
	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, types.NamespacedName{Name: "dave", Namespace: "test-stamping"})
	if updated.Generation == 0 {
		t.Fatalf("metadata.generation is 0 — apiserver should have stamped it on Create")
	}
	if updated.Status.ObservedGeneration != updated.Generation {
		t.Errorf("status.observedGeneration: got %d, want %d (matches metadata.generation after first pod create)",
			updated.Status.ObservedGeneration, updated.Generation)
	}
}

// TestReconciler_NewAgent_CreatingPhase verifies that reconciling a new Agent CRD
// creates the PVC, creates the pod, and moves phase to Creating.
func TestReconciler_NewAgent_CreatingPhase(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-creating"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-creating")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-creating"}}

	// First reconcile: should add finalizer, classify as CRDCreated, create PVC + pod, set Creating.
	reconcileN(t, r, req, 1)

	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-creating"}
	updated := getAgent(t, k8sClient, agentKey)

	if updated.Status.Phase != kyberv1.AgentPhaseCreating {
		t.Errorf("phase: got %q, want %q", updated.Status.Phase, kyberv1.AgentPhaseCreating)
	}
	if !containsString(updated.Finalizers, AgentFinalizer) {
		t.Error("finalizer not set on agent")
	}

	// Verify PVC was created.
	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := types.NamespacedName{Name: PVCName("dave"), Namespace: "test-creating"}
	if err := k8sClient.Get(context.Background(), pvcKey, pvc); err != nil {
		t.Errorf("PVC not found: %v", err)
	}

	// Verify pod was created.
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-creating"}
	if err := k8sClient.Get(context.Background(), podKey, pod); err != nil {
		t.Errorf("pod not found: %v", err)
	}

	// kyber#467: the durable transcript-offsets PVC must be created alongside the
	// persist PVC (the pod spec references it as a volume, so it must exist before
	// the pod is scheduled), owner-referenced to the agent for GC on deletion.
	offsetsPVC := &corev1.PersistentVolumeClaim{}
	offsetsKey := types.NamespacedName{Name: OffsetsPVCName("dave"), Namespace: "test-creating"}
	if err := k8sClient.Get(context.Background(), offsetsKey, offsetsPVC); err != nil {
		t.Fatalf("transcript-offsets PVC not found: %v", err)
	}
	if len(offsetsPVC.OwnerReferences) == 0 || offsetsPVC.OwnerReferences[0].Name != "dave" {
		t.Errorf("offsets PVC must be owner-referenced to the agent (for GC); got %v", offsetsPVC.OwnerReferences)
	}
	if offsetsPVC.Labels["kyber.io/volume"] != "transcript-offsets" {
		t.Errorf("offsets PVC missing distinguishing label; got %v", offsetsPVC.Labels)
	}
	// The pod must actually reference the offsets PVC by claim name.
	var refsOffsets bool
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == OffsetsPVCName("dave") {
			refsOffsets = true
		}
	}
	if !refsOffsets {
		t.Error("pod spec must reference the offsets PVC as a volume")
	}
}

// TestEnsureOffsetsPVC_Idempotent verifies the offsets-PVC ensure is safe to call
// repeatedly (every reconcile hits it) and honors the configured size/class.
func TestEnsureOffsetsPVC_Idempotent(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.TranscriptOffsetsSize = "10Mi"

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-offsets-idem"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("dave", "test-offsets-idem")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	ctx := context.Background()
	if err := r.ensureOffsetsPVC(ctx, agent); err != nil {
		t.Fatalf("first ensureOffsetsPVC: %v", err)
	}
	if err := r.ensureOffsetsPVC(ctx, agent); err != nil {
		t.Fatalf("second ensureOffsetsPVC (idempotent) errored: %v", err)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: OffsetsPVCName("dave"), Namespace: "test-offsets-idem"}
	if err := k8sClient.Get(ctx, key, pvc); err != nil {
		t.Fatalf("offsets PVC not found: %v", err)
	}
	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(resource.MustParse("10Mi")) != 0 {
		t.Errorf("offsets PVC size: got %s, want 10Mi", got.String())
	}
}

// TestCreatePod_EnsuresOffsetsPVC_PreExistingAgent is the kyber#467 review-fix
// (Chewie HOLD on #475) regression guard. An agent that pre-dates the offsets-PVC
// change already has its persist PVC but NO offsets PVC. When its pod is recreated
// via a path OTHER than birth-time ActionCreatePVAndPod — i.e. restart
// (ActionWriteBriefAndCreatePod) or retry (ActionResetRetryAndCreatePod), both of
// which call createPod directly without the birth-time ensure — the offsets PVC
// must still be created, or the pod references a non-existent PVC and is stuck
// Pending forever. The fix puts the ensure inside createPod so EVERY recreation
// path is covered. This test fails (offsets PVC absent → pod references a missing
// claim) if the ensure is removed from createPod.
func TestCreatePod_EnsuresOffsetsPVC_PreExistingAgent(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-preexisting-recreate"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("dave", "test-preexisting-recreate")
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	// Re-fetch so the object carries a UID for owner references.
	agent = getAgent(t, k8sClient, types.NamespacedName{Name: "dave", Namespace: ns.Name})

	// Simulate a PRE-EXISTING agent: the persist PVC already exists, but the
	// offsets PVC (new in #467) does NOT — exactly the state of every agent the
	// moment the controller rolls out with this change.
	persist := BuildPVC(agent, "")
	if err := ctrl.SetControllerReference(agent, persist, scheme); err != nil {
		t.Fatalf("owner ref on persist PVC: %v", err)
	}
	if err := k8sClient.Create(ctx, persist); err != nil {
		t.Fatalf("creating persist PVC: %v", err)
	}
	offKey := types.NamespacedName{Name: OffsetsPVCName("dave"), Namespace: ns.Name}
	if err := k8sClient.Get(ctx, offKey, &corev1.PersistentVolumeClaim{}); !errors.IsNotFound(err) {
		t.Fatalf("precondition: offsets PVC must be absent before recreation, got err=%v", err)
	}

	// Recreate the pod via the direct createPod path (what retry/restart actions do).
	if err := r.createPod(ctx, agent); err != nil {
		t.Fatalf("createPod (recreation): %v", err)
	}

	// The offsets PVC must now exist...
	off := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, offKey, off); err != nil {
		t.Fatalf("offsets PVC must be created by createPod on recreation (else pod stuck Pending): %v", err)
	}
	if len(off.OwnerReferences) == 0 || off.OwnerReferences[0].Name != "dave" {
		t.Errorf("recreated offsets PVC must be owner-referenced to the agent; got %v", off.OwnerReferences)
	}

	// ...and the recreated pod must reference it.
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: AgentPodName("dave"), Namespace: ns.Name}, pod); err != nil {
		t.Fatalf("recreated pod not found: %v", err)
	}
	var refsOffsets bool
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == OffsetsPVCName("dave") {
			refsOffsets = true
		}
	}
	if !refsOffsets {
		t.Error("recreated pod must reference the offsets PVC by claim name")
	}
}

// TestCreatePod_EnsuresPersistPVC_WhenMissing is the companion guard for the
// PERSIST PVC, and it exists because a real agent got permanently stuck without
// it.
//
// On the canary, an agent's persist PVC had to be deleted to correct its
// StorageClass. The reconciler then rebuilt the POD on every reconcile and
// never the CLAIM — because the persist ensure lived only on the birth-time
// ActionCreatePVAndPod path, while retry and restart call createPod directly. The
// agent flapped Starting → Failed → Starting forever on
// "persistentvolumeclaim not found", with no route back short of deleting the
// agent or hand-writing the PVC. The offsets PVC was already immune, having
// been fixed the same way in #467; the persist PVC was not.
//
// This fails if the persist ensure is removed from createPod.
func TestCreatePod_EnsuresPersistPVC_WhenMissing(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-persist-pvc-heal"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("dave", ns.Name)
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agent = getAgent(t, k8sClient, types.NamespacedName{Name: "dave", Namespace: ns.Name})

	persistKey := types.NamespacedName{Name: PVCName("dave"), Namespace: ns.Name}
	if err := k8sClient.Get(ctx, persistKey, &corev1.PersistentVolumeClaim{}); !errors.IsNotFound(err) {
		t.Fatalf("precondition: persist PVC must be absent, got err=%v", err)
	}

	// The recreation path — what retry and restart actually call.
	if err := r.createPod(ctx, agent); err != nil {
		t.Fatalf("createPod (recreation): %v", err)
	}

	persist := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, persistKey, persist); err != nil {
		t.Fatalf("persist PVC must be recreated by createPod, else the agent is stuck Pending forever: %v", err)
	}
	if len(persist.OwnerReferences) == 0 || persist.OwnerReferences[0].Name != "dave" {
		t.Errorf("recreated persist PVC must be owner-referenced to the agent (for GC); got %v", persist.OwnerReferences)
	}

	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: AgentPodName("dave"), Namespace: ns.Name}, pod); err != nil {
		t.Fatalf("recreated pod not found: %v", err)
	}
	var refsPersist bool
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == PVCName("dave") {
			refsPersist = true
		}
	}
	if !refsPersist {
		t.Error("recreated pod must reference the persist PVC by claim name")
	}
}

// TestCreatePod_LeavesExistingPersistPVCAlone guards the other direction: the
// ensure must never touch a claim that already exists. A bound volume carries
// the agent's data, and mutating a PVC's immutable fields fails the reconcile.
func TestCreatePod_LeavesExistingPersistPVCAlone(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-persist-pvc-untouched"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("dave", ns.Name)
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agent = getAgent(t, k8sClient, types.NamespacedName{Name: "dave", Namespace: ns.Name})

	// A pre-existing claim on a DIFFERENT StorageClass — the shape a cluster is
	// in after an operator corrects storage.agentStorageClass. It must survive
	// untouched; storageClassName is immutable and rewriting it would fail.
	existing := BuildPVC(agent, "someone-elses-class")
	if err := ctrl.SetControllerReference(agent, existing, scheme); err != nil {
		t.Fatalf("owner ref: %v", err)
	}
	if err := k8sClient.Create(ctx, existing); err != nil {
		t.Fatalf("creating persist PVC: %v", err)
	}

	r.AgentStorageClass = "a-new-class"
	if err := r.createPod(ctx, agent); err != nil {
		t.Fatalf("createPod: %v", err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: PVCName("dave"), Namespace: ns.Name}, got); err != nil {
		t.Fatalf("persist PVC disappeared: %v", err)
	}
	if got.Spec.StorageClassName == nil || *got.Spec.StorageClassName != "someone-elses-class" {
		t.Errorf("existing PVC was modified: storageClassName = %v, want it left alone", got.Spec.StorageClassName)
	}
}

// TestReconciler_PodReady_RunningPhase verifies that when the pod becomes Ready,
// the agent phase transitions from Starting to Running.
func TestReconciler_PodReady_RunningPhase(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ready"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-ready")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-ready"}}

	// First reconcile: Creating phase, creates PVC + pod.
	reconcileN(t, r, req, 1)

	// Manually set phase to Starting to simulate the Creating → Starting transition.
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-ready"}
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStarting
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching phase to Starting: %v", err)
	}

	// Simulate pod becoming Ready.
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-ready"}
	if err := k8sClient.Get(context.Background(), podKey, pod); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	podPatch := client.MergeFrom(pod.DeepCopy())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	if err := k8sClient.Status().Patch(context.Background(), pod, podPatch); err != nil {
		t.Fatalf("patching pod to Ready: %v", err)
	}

	// Reconcile: should detect pod Ready and transition to Running.
	reconcileN(t, r, req, 1)

	finalAgent := getAgent(t, k8sClient, agentKey)
	if finalAgent.Status.Phase != kyberv1.AgentPhaseRunning {
		t.Errorf("phase: got %q, want %q", finalAgent.Status.Phase, kyberv1.AgentPhaseRunning)
	}
}

// TestReconciler_DesiredStopped_StoppingPhase verifies that setting desiredPhase=Stopped
// on a Running agent transitions it to Stopping.
func TestReconciler_DesiredStopped_StoppingPhase(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-stop"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-stop")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-stop"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-stop"}

	// Bootstrap to Running state.
	reconcileN(t, r, req, 1)

	// Manually fast-forward to Running.
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Set desired phase to Stopped.
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Stopped: %v", err)
	}

	// Reconcile: Running + desired=Stopped → Stopping.
	reconcileN(t, r, req, 1)

	finalAgent := getAgent(t, k8sClient, agentKey)
	if finalAgent.Status.Phase != kyberv1.AgentPhaseStopping {
		t.Errorf("phase: got %q, want %q", finalAgent.Status.Phase, kyberv1.AgentPhaseStopping)
	}
}

// TestReconciler_Deletion_FinalizerCleanup verifies that deleting an Agent CRD
// triggers PVC deletion and finalizer removal.
func TestReconciler_Deletion_FinalizerCleanup(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-delete"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-delete")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-delete"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-delete"}

	// Bootstrap: first reconcile sets up finalizer + PVC + pod, phase → Creating.
	reconcileN(t, r, req, 1)

	// Verify finalizer is present.
	created := getAgent(t, k8sClient, agentKey)
	if !containsString(created.Finalizers, AgentFinalizer) {
		t.Fatal("finalizer not set before deletion test")
	}

	// Delete the agent CRD (sets DeletionTimestamp, finalizer prevents actual removal).
	if err := k8sClient.Delete(context.Background(), created); err != nil {
		t.Fatalf("deleting agent: %v", err)
	}

	// Reconcile: should detect deletion, run finalizer (delete PVC), remove finalizer.
	// First reconcile: deletes the pod (if running) and requeues.
	// Second reconcile: pod gone, deletes PVC, removes finalizer.
	reconcileN(t, r, req, 3)

	// After finalizer runs, the agent should be fully deleted (or finalizer removed).
	finalAgent := &kyberv1.Agent{}
	err := k8sClient.Get(context.Background(), agentKey, finalAgent)
	if err == nil {
		// Agent still exists — finalizer should be removed.
		if containsString(finalAgent.Finalizers, AgentFinalizer) {
			t.Error("finalizer still present after deletion reconcile")
		}
	} else if !errors.IsNotFound(err) {
		t.Fatalf("unexpected error getting agent after deletion: %v", err)
	}
	// If IsNotFound, the agent was fully deleted — that's the expected happy path.
}

// TestReconciler_Deletion_NamespaceTerminating_SelfRemovesFinalizer verifies
// that when the enclosing namespace is itself Terminating (operator nuked the
// namespace), the agent finalizer self-removes instead of running the normal
// pod/PVC/secret cleanup — so the namespace terminator is not blocked.
// See issue #64.
func TestReconciler_Deletion_NamespaceTerminating_SelfRemovesFinalizer(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	// Namespace carries a dummy finalizer so envtest lets us hold it in
	// Terminating after delete (envtest has no namespace terminator).
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-ns-terminating",
			Finalizers: []string{"kubernetes"},
		},
	}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-ns-terminating")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-ns-terminating"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-ns-terminating"}

	// Bootstrap: first reconcile sets up finalizer + PVC + pod.
	reconcileN(t, r, req, 1)
	created := getAgent(t, k8sClient, agentKey)
	if !containsString(created.Finalizers, AgentFinalizer) {
		t.Fatal("finalizer not set before ns-terminating test")
	}

	// Tear the namespace down first — DeletionTimestamp gets set, but the
	// terminator is blocked by our dummy finalizer.
	if err := k8sClient.Delete(context.Background(), ns); err != nil {
		t.Fatalf("deleting namespace: %v", err)
	}
	// Then delete the agent CRD so its finalizer becomes load-bearing.
	if err := k8sClient.Delete(context.Background(), created); err != nil {
		t.Fatalf("deleting agent: %v", err)
	}

	// One reconcile should be enough to see ns.DeletionTimestamp and drop
	// the finalizer without running the normal cleanup path.
	reconcileN(t, r, req, 1)

	// Agent should either be fully gone or have no finalizer left.
	finalAgent := &kyberv1.Agent{}
	err := k8sClient.Get(context.Background(), agentKey, finalAgent)
	if err == nil {
		if containsString(finalAgent.Finalizers, AgentFinalizer) {
			t.Error("finalizer still present after namespace-terminating deletion reconcile")
		}
	} else if !errors.IsNotFound(err) {
		t.Fatalf("unexpected error getting agent: %v", err)
	}
}

// TestReconciler_Restarting_TransitionsToStarting verifies that setting desiredPhase=Restarting
// on a Running agent transitions to Restarting, then Starting after pod deletion.
func TestReconciler_Restarting_TransitionsToStarting(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-restart"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-restart")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-restart"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-restart"}

	// Bootstrap to Creating.
	reconcileN(t, r, req, 1)

	// Fast-forward to Running.
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Set desiredPhase=Restarting.
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Restarting: %v", err)
	}

	// Reconcile: Running + desired=Restarting → Restarting (deletes pod).
	reconcileN(t, r, req, 1)

	agentAfterRestart := getAgent(t, k8sClient, agentKey)
	if agentAfterRestart.Status.Phase != kyberv1.AgentPhaseRestarting {
		t.Errorf("phase after first reconcile: got %q, want %q",
			agentAfterRestart.Status.Phase, kyberv1.AgentPhaseRestarting)
	}

	// Reconcile again: pod should be deleted by now → Restarting + pod deleted → Starting.
	reconcileN(t, r, req, 1)

	agentAfterPodDelete := getAgent(t, k8sClient, agentKey)
	if agentAfterPodDelete.Status.Phase != kyberv1.AgentPhaseStarting {
		t.Errorf("phase after pod deletion: got %q, want %q",
			agentAfterPodDelete.Status.Phase, kyberv1.AgentPhaseStarting)
	}
}

// TestReconciler_FailedAgentBackoffThrottle verifies that a Failed agent with restartCount=1
// is throttled during the 30s backoff window and only creates a new pod after the window elapses.
//
// The test never sleeps — it manipulates LastTransition timestamps directly.
func TestReconciler_FailedAgentBackoffThrottle(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-backoff"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-backoff")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-backoff"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-backoff"}

	// Bootstrap: run first reconcile so finalizer + phase=Creating are set.
	// This also creates the initial pod. We'll delete it to simulate a crash scenario.
	reconcileN(t, r, req, 1)

	// Delete the pod created during bootstrap to simulate it having died (the crash).
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-backoff"}
	existingPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, existingPod); err == nil {
		if err := k8sClient.Delete(context.Background(), existingPod); err != nil && !errors.IsNotFound(err) {
			t.Fatalf("deleting bootstrap pod: %v", err)
		}
	}

	// Force agent into Failed with restartCount=1 and a very recent LastTransition
	// (simulating a crash that just happened — well within the 30s backoff window).
	recentFailure := metav1.NewTime(time.Now().Add(-1 * time.Second))
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	updated.Status.Phase = kyberv1.AgentPhaseFailed
	updated.Status.RestartCount = 1
	updated.Status.LastTransition = &recentFailure
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching agent to Failed: %v", err)
	}

	// Reconcile: should be throttled — backoff for restartCount=1 is 30s, only 1s has elapsed.
	ctx := context.Background()
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile (throttled) failed: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 during backoff window, got 0")
	}
	// The remaining backoff should be close to 30s (we set LastTransition 1s ago, so ~29s remaining).
	const backoffForCount1 = 30 * time.Second
	if result.RequeueAfter > backoffForCount1 {
		t.Errorf("RequeueAfter=%v is larger than the full backoff %v", result.RequeueAfter, backoffForCount1)
	}

	// Confirm no pod was created during the throttled reconcile.
	pod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, pod); err == nil {
		t.Error("pod was created during backoff window — throttle did not fire")
	} else if !errors.IsNotFound(err) {
		t.Fatalf("unexpected error checking pod: %v", err)
	}

	// Agent phase must still be Failed (no transition happened).
	agentMid := getAgent(t, k8sClient, agentKey)
	if agentMid.Status.Phase != kyberv1.AgentPhaseFailed {
		t.Errorf("phase during throttle: got %q, want %q", agentMid.Status.Phase, kyberv1.AgentPhaseFailed)
	}

	// Fast-forward: move LastTransition far enough into the past to clear the backoff window.
	pastFailure := metav1.NewTime(time.Now().Add(-60 * time.Second))
	agentToUpdate := getAgent(t, k8sClient, agentKey)
	patch2 := client.MergeFrom(agentToUpdate.DeepCopy())
	agentToUpdate.Status.LastTransition = &pastFailure
	if err := k8sClient.Status().Patch(context.Background(), agentToUpdate, patch2); err != nil {
		t.Fatalf("patching LastTransition to past: %v", err)
	}

	// Reconcile again: backoff window has elapsed — auto-restart should proceed.
	result2, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile (after backoff) failed: %v", err)
	}
	// After the restart fires, the reconciler sets RequeueAfter=requeueWaiting (30s) to
	// wait for the new pod. It should NOT be the backoff throttle value.
	_ = result2 // the exact value depends on action; we care that no throttle fired.

	// The agent must have transitioned out of Failed (to Starting).
	agentFinal := getAgent(t, k8sClient, agentKey)
	if agentFinal.Status.Phase != kyberv1.AgentPhaseStarting {
		t.Errorf("phase after backoff elapsed: got %q, want %q", agentFinal.Status.Phase, kyberv1.AgentPhaseStarting)
	}

	// A new pod must now exist.
	pod2 := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, pod2); err != nil {
		t.Errorf("pod not found after backoff elapsed: %v", err)
	}
}

// TestReconciler_StartTimeIsPersisted verifies that status.startTime is persisted to the API server
// when an agent transitions to Running. A fresh Get after the reconcile must show startTime != nil.
func TestReconciler_StartTimeIsPersisted(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-starttime"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-starttime")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-starttime"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-starttime"}

	// First reconcile: Creating phase, creates PVC + pod.
	reconcileN(t, r, req, 1)

	// Fast-forward agent to Starting.
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStarting
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Starting: %v", err)
	}

	// Simulate pod becoming Ready.
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-starttime"}
	if err := k8sClient.Get(context.Background(), podKey, pod); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	podPatch := client.MergeFrom(pod.DeepCopy())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	if err := k8sClient.Status().Patch(context.Background(), pod, podPatch); err != nil {
		t.Fatalf("patching pod to Ready: %v", err)
	}

	// Reconcile: Starting + PodReady → Running. updatePhase must persist StartTime.
	reconcileN(t, r, req, 1)

	// Fresh Get from the cluster — not the in-memory object.
	final := getAgent(t, k8sClient, agentKey)
	if final.Status.Phase != kyberv1.AgentPhaseRunning {
		t.Fatalf("phase: got %q, want Running", final.Status.Phase)
	}
	if final.Status.StartTime == nil {
		t.Error("status.startTime is nil after transitioning to Running — fix: set StartTime inside updatePhase, not in executeAction")
	}
}

// TestReconciler_RestartingClearsDesiredPhase verifies that after a desired Restarting cycle
// completes, spec.desiredPhase is cleared to "" so the next reconcile does NOT trigger another restart.
func TestReconciler_RestartingClearsDesiredPhase(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-restart-clear"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-restart-clear")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-restart-clear"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-restart-clear"}

	// Bootstrap: Creating phase, creates PVC + pod.
	reconcileN(t, r, req, 1)

	// Fast-forward to Running.
	updated := getAgent(t, k8sClient, agentKey)
	statusPatch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, statusPatch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Set desiredPhase=Restarting.
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Restarting: %v", err)
	}

	// Reconcile: Running + desired=Restarting → Restarting (deletes pod, clears desiredPhase).
	reconcileN(t, r, req, 1)

	afterFirstReconcile := getAgent(t, k8sClient, agentKey)
	if afterFirstReconcile.Status.Phase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("phase after restart trigger: got %q, want Restarting", afterFirstReconcile.Status.Phase)
	}
	// desiredPhase must already be cleared so the next reconcile doesn't re-trigger.
	if afterFirstReconcile.Spec.DesiredPhase != "" {
		t.Errorf("spec.desiredPhase after Restarting trigger: got %q, want \"\" — infinite restart loop bug", afterFirstReconcile.Spec.DesiredPhase)
	}

	// Reconcile: Restarting + pod deleted → Starting (creates new pod).
	reconcileN(t, r, req, 1)

	afterPodDelete := getAgent(t, k8sClient, agentKey)
	if afterPodDelete.Status.Phase != kyberv1.AgentPhaseStarting {
		t.Fatalf("phase after pod delete: got %q, want Starting", afterPodDelete.Status.Phase)
	}

	// Fast-forward to Running again (simulate pod becoming Ready).
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-restart-clear"}
	if err := k8sClient.Get(context.Background(), podKey, pod); err != nil {
		t.Fatalf("getting new pod: %v", err)
	}
	podPatch2 := client.MergeFrom(pod.DeepCopy())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	if err := k8sClient.Status().Patch(context.Background(), pod, podPatch2); err != nil {
		t.Fatalf("patching new pod to Ready: %v", err)
	}
	reconcileN(t, r, req, 1)

	afterRunning := getAgent(t, k8sClient, agentKey)
	if afterRunning.Status.Phase != kyberv1.AgentPhaseRunning {
		t.Fatalf("phase after restart completes: got %q, want Running", afterRunning.Status.Phase)
	}

	// Record pod name before the extra reconcile.
	podNameBefore := AgentPodName("dave")

	// Reconcile once more — desiredPhase is "" so no restart should trigger.
	reconcileN(t, r, req, 1)

	afterExtraReconcile := getAgent(t, k8sClient, agentKey)
	if afterExtraReconcile.Status.Phase != kyberv1.AgentPhaseRunning {
		t.Errorf("phase after extra reconcile: got %q, want Running (should not have restarted)", afterExtraReconcile.Status.Phase)
	}

	// The pod should still exist and have the same name (no deletion happened).
	pod2 := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: podNameBefore, Namespace: "test-restart-clear"}, pod2); err != nil {
		t.Errorf("pod not found after extra reconcile — was the agent restarted again? %v", err)
	}
}

// TestReconciler_FirstBoot_BriefWritten verifies that on first boot (Creating → Starting),
// a session brief is written to the BriefStore with shutdown_type=planned and restart_reason=first_boot.
func TestReconciler_FirstBoot_BriefWritten(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	store := briefstore.NewMemoryStore()
	r := newReconcilerWithStore(k8sClient, scheme, store)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-brief-firstboot"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-brief-firstboot")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-brief-firstboot"}}

	// First reconcile: EventCRDCreated → ActionCreatePVAndPod (no brief write — first boot uses ActionCreatePVAndPod, not ActionWriteBriefAndCreatePod).
	// The brief is only written in ActionWriteBriefAndCreatePod.
	// For a brand-new agent, the state machine fires EventCRDCreated → ActionCreatePVAndPod.
	// We need to drive the reconciler to a state where ActionWriteBriefAndCreatePod fires.
	// Simulate: agent goes to Stopped, then desired=Running → ActionWriteBriefAndCreatePod.
	reconcileN(t, r, req, 1)

	// Fast-forward to Stopped (simulating a stop cycle).
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-brief-firstboot"}
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStopped
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Stopped: %v", err)
	}

	// Delete the pod from first boot so ActionWriteBriefAndCreatePod can recreate it.
	existingPod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-brief-firstboot"}
	if err := k8sClient.Get(context.Background(), podKey, existingPod); err == nil {
		if err := k8sClient.Delete(context.Background(), existingPod); err != nil {
			t.Logf("deleting pod (may already be gone): %v", err)
		}
	}

	// Set desiredPhase=Running to trigger the Stopped → Starting transition (ActionWriteBriefAndCreatePod).
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Running: %v", err)
	}

	// Reconcile: Stopped + desired=Running → ActionWriteBriefAndCreatePod → Starting.
	reconcileN(t, r, req, 1)

	// Verify the brief was written.
	ctx := context.Background()
	brief, err := store.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("brief not found in store: %v", err)
	}

	if brief.ShutdownType != "planned" {
		t.Errorf("ShutdownType: got %q, want %q", brief.ShutdownType, "planned")
	}
	if brief.RestartReason != "operator" {
		t.Errorf("RestartReason: got %q, want %q", brief.RestartReason, "operator")
	}
	if brief.AgentName != "dave" {
		t.Errorf("AgentName: got %q, want %q", brief.AgentName, "dave")
	}
	if brief.Version != 1 {
		t.Errorf("Version: got %d, want 1", brief.Version)
	}
}

// TestReconciler_ResetRetryWritesBrief verifies that ActionResetRetryAndCreatePod writes a session
// brief before creating the pod. This covers the case where an operator sets spec.desiredPhase=Running
// on a Failed agent that has exhausted its automatic retry limit.
func TestReconciler_ResetRetryWritesBrief(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	store := briefstore.NewMemoryStore()
	r := newReconcilerWithStore(k8sClient, scheme, store)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-reset-retry-brief"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-reset-retry-brief")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-reset-retry-brief"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-reset-retry-brief"}

	// Bootstrap: first reconcile sets up finalizer + PVC + pod, phase → Creating.
	reconcileN(t, r, req, 1)

	// Delete the bootstrap pod so ActionResetRetryAndCreatePod can create a fresh one.
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-reset-retry-brief"}
	existingPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, existingPod); err == nil {
		if err := k8sClient.Delete(context.Background(), existingPod); err != nil && !errors.IsNotFound(err) {
			t.Fatalf("deleting bootstrap pod: %v", err)
		}
	}

	// Force agent into Failed with RestartCount=3 (retry limit exhausted) and a stale
	// LastTransition so the backoff check does not throttle the reconcile.
	pastFailure := metav1.NewTime(time.Now().Add(-120 * time.Second))
	updated := getAgent(t, k8sClient, agentKey)
	statusPatch := client.MergeFrom(updated.DeepCopy())
	updated.Status.Phase = kyberv1.AgentPhaseFailed
	updated.Status.RestartCount = 3
	updated.Status.LastTransition = &pastFailure
	if err := k8sClient.Status().Patch(context.Background(), updated, statusPatch); err != nil {
		t.Fatalf("patching agent to Failed with RestartCount=3: %v", err)
	}

	// Operator override: set spec.desiredPhase=Running to trigger ActionResetRetryAndCreatePod.
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Running: %v", err)
	}

	// Reconcile: Failed + desired=Running + RetryCount>=max → ActionResetRetryAndCreatePod.
	reconcileN(t, r, req, 1)

	// Assert pod was created.
	pod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, pod); err != nil {
		t.Errorf("pod not found after operator restart: %v", err)
	}

	// Assert RestartCount was reset to 0.
	finalAgent := getAgent(t, k8sClient, agentKey)
	if finalAgent.Status.RestartCount != 0 {
		t.Errorf("RestartCount: got %d, want 0", finalAgent.Status.RestartCount)
	}

	// Assert the brief was written with the correct fields.
	ctx := context.Background()
	brief, err := store.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("brief not found in store after operator restart: %v", err)
	}

	if brief.ShutdownType != "planned" {
		t.Errorf("ShutdownType: got %q, want %q", brief.ShutdownType, "planned")
	}
	if brief.RestartReason != "operator" {
		t.Errorf("RestartReason: got %q, want %q", brief.RestartReason, "operator")
	}
	if brief.AgentName != "dave" {
		t.Errorf("AgentName: got %q, want %q", brief.AgentName, "dave")
	}
	if brief.Version != 1 {
		t.Errorf("Version: got %d, want 1", brief.Version)
	}
}

// TestReconciler_ReauthReplacesStaleFailedPod verifies that when an agent re-auths out
// of NeedsAuth while a Failed pod from the prior OAuth-refresh-failure attempt is still
// present, the reconciler sweeps the stale pod and creates a fresh one. This guards
// against the regression where createPod silently swallowed AlreadyExists, leaving the
// fresh refresh_token in the Secret permanently unread and flipping the agent
// Starting ↔ NeedsAuth forever.
func TestReconciler_ReauthReplacesStaleFailedPod(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-reauth-stale-pod"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-reauth-stale-pod")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-reauth-stale-pod"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-reauth-stale-pod"}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-reauth-stale-pod"}

	// Bootstrap: first reconcile creates PVC + initial pod, phase → Creating.
	reconcileN(t, r, req, 1)

	// Capture the original pod's UID so we can later verify a fresh pod replaced it.
	originalPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, originalPod); err != nil {
		t.Fatalf("getting original pod: %v", err)
	}
	originalUID := originalPod.UID

	// Simulate the OAuth-refresh-failure outcome the node-agent would report:
	// pod exits with code 2, lands in Failed phase; agent transitions to NeedsAuth.
	podPatch := client.MergeFrom(originalPod.DeepCopy())
	originalPod.Status.Phase = corev1.PodFailed
	originalPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "agent",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"},
		},
	}}
	if err := k8sClient.Status().Patch(context.Background(), originalPod, podPatch); err != nil {
		t.Fatalf("patching pod to Failed: %v", err)
	}

	agentObj := getAgent(t, k8sClient, agentKey)
	statusPatch := client.MergeFrom(agentObj.DeepCopy())
	now := metav1.Now()
	agentObj.Status.Phase = kyberv1.AgentPhaseNeedsAuth
	agentObj.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), agentObj, statusPatch); err != nil {
		t.Fatalf("patching agent to NeedsAuth: %v", err)
	}

	// Operator re-authorizes: /api/v1/agents/{name}/oauth writes the new tokens
	// and sets desiredPhase=Running, which fires NeedsAuth → Starting
	// (ActionResetRetryAndCreatePod) on the next reconcile.
	agentObj = getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Running: %v", err)
	}

	// Two reconciles: first sweeps the stale Failed pod and creates the replacement;
	// if the first pass can only do one of the two, the second completes the work.
	reconcileN(t, r, req, 2)

	replacement := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, replacement); err != nil {
		t.Fatalf("fresh pod not found after re-auth: %v", err)
	}
	if replacement.UID == originalUID {
		t.Errorf("pod UID unchanged: stale Failed pod was not swept (UID=%s, phase=%s)",
			replacement.UID, replacement.Status.Phase)
	}
	if replacement.Status.Phase == corev1.PodFailed {
		t.Errorf("replacement pod is still in Failed phase — stale pod carried over")
	}
}

// TestReconciler_AuthoritativeStop_HaltsCrashLoopingAgent is the headline #468
// regression: an operator sets desiredPhase=Stopped on a crash-looping agent
// (Failed, RestartCount < maxRestartRetries, backoff throttle expired — i.e. it
// WOULD auto-restart) and the controller must STOP recreating the pod. It
// converges to Stopped and stays there across a resync, instead of the
// 2026-06-06 incident behavior where Stop-from-Failed was ignored and the
// controller fought the operator by auto-restarting. Twin of
// TestReconciler_ReauthReplacesStaleFailedPod (the #395 kill switch).
func TestReconciler_AuthoritativeStop_HaltsCrashLoopingAgent(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-stop-crashloop"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-stop-crashloop")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-stop-crashloop"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-stop-crashloop"}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-stop-crashloop"}

	// Bootstrap: first reconcile creates PVC + initial pod.
	reconcileN(t, r, req, 1)

	// Simulate the crash: the pod died (terminal), and the agent is parked in
	// Failed with RestartCount BELOW the retry budget and a stale LastTransition,
	// so the auto-restart path is armed (without Stop, the next reconcile would
	// fire EventAutoRestartTriggered and recreate the pod — the incident loop).
	originalPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, originalPod); err != nil {
		t.Fatalf("getting bootstrap pod: %v", err)
	}
	originalUID := originalPod.UID
	podPatch := client.MergeFrom(originalPod.DeepCopy())
	originalPod.Status.Phase = corev1.PodFailed
	if err := k8sClient.Status().Patch(context.Background(), originalPod, podPatch); err != nil {
		t.Fatalf("patching pod to Failed: %v", err)
	}

	pastFailure := metav1.NewTime(time.Now().Add(-120 * time.Second))
	updated := getAgent(t, k8sClient, agentKey)
	statusPatch := client.MergeFrom(updated.DeepCopy())
	updated.Status.Phase = kyberv1.AgentPhaseFailed
	updated.Status.RestartCount = 1 // < maxRestartRetries (3): auto-restart armed
	updated.Status.LastTransition = &pastFailure
	if err := k8sClient.Status().Patch(context.Background(), updated, statusPatch); err != nil {
		t.Fatalf("patching agent to Failed: %v", err)
	}

	// Operator hits the kill switch: desiredPhase=Stopped.
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Stopped: %v", err)
	}

	// Drive the reconcile to convergence: Failed + desired=Stopped →
	// CaptureStateAndDeletePod → Stopping → (pod terminal/gone) → Stopped.
	reconcileN(t, r, req, 4)

	finalAgent := getAgent(t, k8sClient, agentKey)
	if finalAgent.Status.Phase != kyberv1.AgentPhaseStopped {
		t.Errorf("phase: got %q, want %q (Stop must halt the crash loop, not auto-restart)",
			finalAgent.Status.Phase, kyberv1.AgentPhaseStopped)
	}

	// The pod must NOT have been auto-restarted: no fresh pod (new UID) exists.
	// (The original terminal pod may linger as Terminating in envtest, which has
	// no kubelet to finalize the delete — that's fine; what must never happen is
	// a NEW pod being created to replace it.)
	assertNotRecreated := func(stage string) {
		pod := &corev1.Pod{}
		err := k8sClient.Get(context.Background(), podKey, pod)
		if err != nil {
			if errors.IsNotFound(err) {
				return // deleted and gone — ideal
			}
			t.Fatalf("%s: getting pod: %v", stage, err)
		}
		if pod.UID != originalUID {
			t.Errorf("%s: a fresh pod (UID=%s) was created — Stop failed to pre-empt auto-restart/recreate", stage, pod.UID)
		}
		if pod.DeletionTimestamp == nil {
			t.Errorf("%s: original pod still live with no deletion timestamp — Stop did not delete it", stage)
		}
	}
	assertNotRecreated("after convergence")

	// Stays down across a resync: extra reconciles (periodic requeue) must not
	// recreate the pod, and the phase must remain Stopped — a stable fixed point
	// until desiredPhase flips back to Running.
	reconcileN(t, r, req, 3)
	resynced := getAgent(t, k8sClient, agentKey)
	if resynced.Status.Phase != kyberv1.AgentPhaseStopped {
		t.Errorf("after resync: phase drifted to %q, want it to stay %q", resynced.Status.Phase, kyberv1.AgentPhaseStopped)
	}
	assertNotRecreated("after resync")
}

// TestReconciler_ResetRetry_SweepsLivePod is the regression test for #149.
//
// Scenario: an agent is in Failed phase but its pod is still Pending (e.g. it
// was unschedulable due to a too-large memory request). The operator calls
// /set-resources to lower the request; the API patches spec.resources AND sets
// spec.desiredPhase=Running. The next reconcile fires Failed+EventDesiredRunning
// → ActionResetRetryAndCreatePod. Before the fix, the action's createPod step
// hit the safety guard at reconciler.go:899-908 ("cannot create pod: existing
// pod in non-terminal phase Pending") and the reconciler requeued forever — the
// stale Pending pod with the old resource request never went away without a
// manual `kubectl delete pod`.
//
// After the fix, the action sweeps any existing pod (terminal or live) before
// calling createPod, so the new pod lands with the patched resources.
func TestReconciler_ResetRetry_SweepsLivePod(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-resetretry-live-pod"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-resetretry-live-pod")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-resetretry-live-pod"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-resetretry-live-pod"}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-resetretry-live-pod"}

	// Bootstrap: first reconcile creates PVC + initial pod. The pod stays
	// Pending in envtest (no scheduler) — that's the "live" pod we want
	// ResetRetry to sweep.
	reconcileN(t, r, req, 1)

	originalPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, originalPod); err != nil {
		t.Fatalf("getting bootstrap pod: %v", err)
	}
	if originalPod.Status.Phase == corev1.PodFailed || originalPod.Status.Phase == corev1.PodSucceeded {
		t.Fatalf("bootstrap pod is in terminal phase %q; this test needs a live (non-terminal) pod to exercise the #149 sweep", originalPod.Status.Phase)
	}
	originalUID := originalPod.UID
	originalMemory := originalPod.Spec.Containers[0].Resources.Requests.Memory().String()

	// Force the agent to Failed with a stale LastTransition so the backoff
	// throttle does not apply. Pod stays Pending. Mirrors the #149 production
	// repro (alice in kyber-pr-147): unschedulable pod, agent eventually Failed.
	pastFailure := metav1.NewTime(time.Now().Add(-120 * time.Second))
	updated := getAgent(t, k8sClient, agentKey)
	statusPatch := client.MergeFrom(updated.DeepCopy())
	updated.Status.Phase = kyberv1.AgentPhaseFailed
	updated.Status.RestartCount = 3
	updated.Status.LastTransition = &pastFailure
	if err := k8sClient.Status().Patch(context.Background(), updated, statusPatch); err != nil {
		t.Fatalf("patching agent to Failed: %v", err)
	}

	// Operator calls /set-resources: patches spec.resources to a smaller value
	// AND sets spec.desiredPhase=Running (because agent is Failed). See
	// pkg/api/routes_agents.go:setAgentResources lines 957-963.
	newMemory := resource.MustParse("128Mi")
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.Resources.Memory = newMemory
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("patching spec.resources + spec.desiredPhase: %v", err)
	}

	// Reconcile: Failed + EventDesiredRunning → ActionResetRetryAndCreatePod.
	// Before the fix this errors out because the stale Pending pod blocks
	// createPod's safety guard.
	reconcileN(t, r, req, 1)

	// The pod must have been replaced with a fresh one carrying the new memory request.
	freshPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, freshPod); err != nil {
		t.Fatalf("expected a replacement pod after ResetRetry, got error: %v", err)
	}
	if freshPod.UID == originalUID {
		t.Errorf("pod UID unchanged: stale Pending pod was not swept (UID=%s)", freshPod.UID)
	}
	gotMemory := freshPod.Spec.Containers[0].Resources.Requests.Memory().String()
	if gotMemory != newMemory.String() {
		t.Errorf("replacement pod memory request: got %q, want %q (was %q on stale pod)",
			gotMemory, newMemory.String(), originalMemory)
	}

	// Agent must have transitioned out of Failed.
	finalAgent := getAgent(t, k8sClient, agentKey)
	if finalAgent.Status.Phase == kyberv1.AgentPhaseFailed {
		t.Errorf("agent still in Failed phase; expected Starting (or beyond)")
	}
}

// TestReconciler_CreatePodRefusesLivePod verifies that createPod refuses to clobber
// a live (Pending / Running) pod. Callers of createPod — the state-machine action
// handlers — are expected to produce a clean slate; a live pod here indicates an
// upstream state-machine bug that the previous silent-AlreadyExists behavior would
// have masked.
func TestReconciler_CreatePodRefusesLivePod(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-createpod-live"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-createpod-live")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-createpod-live"}}
	reconcileN(t, r, req, 1)

	// Bootstrap pod is Pending (envtest has no scheduler); invoking createPod directly
	// should refuse to overwrite it.
	agentObj := getAgent(t, k8sClient, types.NamespacedName{Name: "dave", Namespace: "test-createpod-live"})
	err := r.createPod(context.Background(), agentObj)
	if err == nil {
		t.Fatal("expected createPod to fail against live pod, got nil")
	}
}

// TestReconciler_AutoCreatePending_GatesPodCreation verifies that when an
// agent has spec.identityRepo.template set but spec.identityRepo.repo is still
// empty (scaffolder hasn't patched it yet), Reconcile returns early with a
// short requeue and does NOT run the state machine. Without this gate, the
// pod would be built before the identity-repo env vars/mount are known, and
// a subsequent scaffold success would never propagate into the running pod
// (spec changes don't rebuild pods).
//
// Reproduces the hk-47 incident: PWA-driven auto-create raced GitHub's async
// template-generate, the first reconcile errored out on GET /git/trees
// returning 409 "Git Repository is empty", and the state machine still built
// a pod with no KYBER_IDENTITY_REPO env var.
func TestReconciler_AutoCreatePending_GatesPodCreation(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.GithubTokenMinter = &fakeMinter{
		tok: &githubapp.InstallationToken{Token: "ghs_test", ExpiresAt: time.Now().Add(time.Hour)},
	}
	// Scaffolder returns an error — simulates the real-world case where the
	// scaffold hasn't completed yet (409 empty-repo, network blip, etc.).
	r.Scaffolder = &fakeScaffolder{err: fmt.Errorf("GitHub 409: Git Repository is empty")}
	r.IdentityRepoOwner = "matty-v"

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-autocreate-gate"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("hk-47", "test-autocreate-gate")
	agent.Spec.IdentityRepo = kyberv1.AgentIdentityRepo{
		Template: "matty-v/kyber-agent-template",
		// Repo intentionally empty.
	}
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "hk-47", Namespace: "test-autocreate-gate"}}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter: got %v, want positive duration (gate should requeue)", res.RequeueAfter)
	}

	// Assert no pod was created.
	podKey := types.NamespacedName{Name: AgentPodName("hk-47"), Namespace: "test-autocreate-gate"}
	pod := &corev1.Pod{}
	getErr := k8sClient.Get(context.Background(), podKey, pod)
	if getErr == nil {
		t.Errorf("pod %s was created despite pending auto-create scaffold", podKey.Name)
	} else if !errors.IsNotFound(getErr) {
		t.Errorf("unexpected error getting pod: %v", getErr)
	}

	// Agent phase should not have progressed beyond the initial empty/Creating state.
	got := getAgent(t, k8sClient, types.NamespacedName{Name: "hk-47", Namespace: "test-autocreate-gate"})
	if got.Status.Phase != "" && got.Status.Phase != kyberv1.AgentPhaseCreating {
		t.Errorf("phase: got %q, want empty or Creating (state machine should be gated)", got.Status.Phase)
	}
}

// TestReconciler_RestartFromStopped_BriefWritten verifies that when an agent restarts from Stopped,
// a brief is written with shutdown_type=planned and restart_reason=operator.
func TestReconciler_RestartFromStopped_BriefWritten(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	store := briefstore.NewMemoryStore()
	r := newReconcilerWithStore(k8sClient, scheme, store)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-brief-stopped"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-brief-stopped")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-brief-stopped"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-brief-stopped"}

	// Bootstrap: first reconcile creates PVC + pod.
	reconcileN(t, r, req, 1)

	// Fast-forward directly to Stopped.
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStopped
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Stopped: %v", err)
	}

	// Remove the pod from the bootstrap so ActionWriteBriefAndCreatePod creates a fresh one.
	existingPod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-brief-stopped"}
	if err := k8sClient.Get(context.Background(), podKey, existingPod); err == nil {
		if err := k8sClient.Delete(context.Background(), existingPod); err != nil {
			t.Logf("deleting pod (may already be gone): %v", err)
		}
	}

	// Set desiredPhase=Running.
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Running: %v", err)
	}

	// Reconcile: Stopped + desired=Running → ActionWriteBriefAndCreatePod → Starting.
	reconcileN(t, r, req, 1)

	ctx := context.Background()
	brief, err := store.Get(ctx, "dave")
	if err != nil {
		t.Fatalf("brief not found in store: %v", err)
	}

	if brief.ShutdownType != "planned" {
		t.Errorf("ShutdownType: got %q, want %q", brief.ShutdownType, "planned")
	}
	if brief.RestartReason != "operator" {
		t.Errorf("RestartReason: got %q, want %q", brief.RestartReason, "operator")
	}
	// Verify the agent phase moved to Starting.
	finalAgent := getAgent(t, k8sClient, agentKey)
	if finalAgent.Status.Phase != kyberv1.AgentPhaseStarting {
		t.Errorf("phase: got %q, want Starting", finalAgent.Status.Phase)
	}
}

// fakeMachineGetter is a test double for MachineGetter that returns a fixed machine or error.
type fakeMachineGetter struct {
	machine *kyberv1.Machine
	err     error
}

func (f *fakeMachineGetter) Get(_ context.Context, _, _ string) (*kyberv1.Machine, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.machine, nil
}

// TestReconciler_UnavailableMachineParksAndResumesAgent is the end-to-end
// controller regression for managed-capacity loss. A Pending pod must be
// removed and parked without spending retries, then rebuilt automatically on
// the replacement node when the Machine becomes Ready.
func TestReconciler_UnavailableMachineParksAndResumesAgent(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	ctx := context.Background()
	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &KubernetesMachineGetter{Client: k8sClient}
	namespace := "test-machine-recovery"
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: namespace},
		Spec: kyberv1.MachineSpec{
			Provider: kyberv1.MachineProviderFake, DesiredPhase: kyberv1.MachinePhaseRunning,
		},
	}
	if err := k8sClient.Create(ctx, machine); err != nil {
		t.Fatalf("creating machine: %v", err)
	}
	machinePatch := client.MergeFrom(machine.DeepCopy())
	machine.Status.Phase = kyberv1.MachinePhaseProvisioning
	machine.Status.NodeName = "old-node"
	if err := k8sClient.Status().Patch(ctx, machine, machinePatch); err != nil {
		t.Fatalf("setting machine unavailable: %v", err)
	}

	agent := newTestAgent("dave", namespace)
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	key := types.NamespacedName{Name: agent.Name, Namespace: namespace}
	req := ctrl.Request{NamespacedName: key}
	reconcileN(t, r, req, 1) // birth path creates PVCs and the initial pod

	current := getAgent(t, k8sClient, key)
	statusPatch := client.MergeFrom(current.DeepCopy())
	current.Status.Scheduling = &kyberv1.AgentSchedulingStatus{
		Category: "Placement", LastError: "raw scheduler detail",
	}
	if err := k8sClient.Status().Patch(ctx, current, statusPatch); err != nil {
		t.Fatalf("seeding scheduling status: %v", err)
	}

	reconcileN(t, r, req, 1)
	parked := getAgent(t, k8sClient, key)
	if parked.Status.Phase != kyberv1.AgentPhaseWaitingForMachine {
		t.Fatalf("phase after capacity loss: got %q, want WaitingForMachine", parked.Status.Phase)
	}
	if parked.Status.RestartCount != 0 {
		t.Errorf("restartCount after capacity loss: got %d, want 0", parked.Status.RestartCount)
	}
	if parked.Status.Scheduling != nil {
		t.Errorf("stale scheduling status was not cleared: %+v", parked.Status.Scheduling)
	}
	if !strings.Contains(parked.Status.Message, "resume this agent automatically") {
		t.Errorf("waiting message = %q", parked.Status.Message)
	}
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: AgentPodName(agent.Name), Namespace: namespace}, pod); !errors.IsNotFound(err) {
		t.Fatalf("old pod still exists after parking: %v", err)
	}

	machine = &kyberv1.Machine{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "node-01", Namespace: namespace}, machine); err != nil {
		t.Fatalf("getting machine for recovery: %v", err)
	}
	machinePatch = client.MergeFrom(machine.DeepCopy())
	machine.Status.Phase = kyberv1.MachinePhaseReady
	machine.Status.NodeName = "replacement-node"
	if err := k8sClient.Status().Patch(ctx, machine, machinePatch); err != nil {
		t.Fatalf("setting machine ready: %v", err)
	}

	reconcileN(t, r, req, 1)
	resumed := getAgent(t, k8sClient, key)
	if resumed.Status.Phase != kyberv1.AgentPhaseStarting {
		t.Fatalf("phase after replacement: got %q, want Starting", resumed.Status.Phase)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: AgentPodName(agent.Name), Namespace: namespace}, pod); err != nil {
		t.Fatalf("replacement pod was not created: %v", err)
	}
	values := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values
	if len(values) != 1 || values[0] != "replacement-node" {
		t.Errorf("replacement pod node affinity = %v, want replacement-node", values)
	}
}

// TestIsMachinePreempted_PreemptionPhases verifies that isMachinePreempted returns true when
// the machine is in Preempted, Replacing, or Provisioning phase.
func TestIsMachinePreempted_PreemptionPhases(t *testing.T) {
	ctx := context.Background()
	r := &AgentReconciler{}

	agent := &kyberv1.Agent{
		Spec: kyberv1.AgentSpec{Machine: "node-01"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
		},
	}

	for _, phase := range []kyberv1.MachinePhase{
		kyberv1.MachinePhasePreempted,
		kyberv1.MachinePhaseReplacing,
		kyberv1.MachinePhaseProvisioning,
	} {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			r.MachineGetter = &fakeMachineGetter{
				machine: &kyberv1.Machine{
					Status: kyberv1.MachineStatus{Phase: phase},
				},
			}
			if !r.isMachinePreempted(ctx, agent, nil) {
				t.Errorf("isMachinePreempted: got false, want true for phase %q", phase)
			}
		})
	}
}

// TestIsMachinePreempted_NilMachineGetter verifies that isMachinePreempted returns false
// when MachineGetter is nil (pre-preemption behaviour, never classifies as preempted).
func TestIsMachinePreempted_NilMachineGetter(t *testing.T) {
	ctx := context.Background()
	r := &AgentReconciler{MachineGetter: nil}

	agent := &kyberv1.Agent{
		Spec:       kyberv1.AgentSpec{Machine: "node-01"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
	}

	if r.isMachinePreempted(ctx, agent, nil) {
		t.Error("isMachinePreempted: got true, want false when MachineGetter is nil")
	}
}

func TestClassifyEvent_FailedAgentRetainsFailureWhileMachineUnavailable(t *testing.T) {
	r := &AgentReconciler{
		MachineGetter: &fakeMachineGetter{machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseProvisioning},
		}},
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "default"},
		Spec:       kyberv1.AgentSpec{Machine: "node-01"},
		Status: kyberv1.AgentStatus{
			Phase:        kyberv1.AgentPhaseFailed,
			RestartCount: maxRestartRetries,
		},
	}
	event, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventRetryLimitReached {
		t.Fatalf("event = %q, want RetryLimitReached; an unrelated Machine outage must not revive a failed Agent", event)
	}
}

func TestClassifyEvent_WaitingForMachineHonorsStop(t *testing.T) {
	r := &AgentReconciler{}
	agent := &kyberv1.Agent{
		Spec: kyberv1.AgentSpec{DesiredPhase: kyberv1.AgentPhaseStopped},
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseWaitingForMachine,
		},
	}
	event, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventDesiredStopped {
		t.Fatalf("event = %q, want DesiredStopped", event)
	}
}

func TestIsNodeUnavailable(t *testing.T) {
	r := newFakeReconcilerWithNodes(t, readyNode("ready"), notReadyNode("not-ready"))
	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "ready", want: false},
		{name: "not-ready", want: true},
		{name: "missing", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.isNodeUnavailable(context.Background(), tc.name); got != tc.want {
				t.Errorf("isNodeUnavailable(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestIsMachinePreempted_NonPreemptionPhases verifies that isMachinePreempted returns false
// for phases that are not preemption-related (Ready, Running, Stopped, etc.).
// MachinePhaseFailed is excluded here because its result depends on Spec.Spot — see the
// dedicated spot/non-spot tests below.
func TestIsMachinePreempted_NonPreemptionPhases(t *testing.T) {
	ctx := context.Background()

	for _, phase := range []kyberv1.MachinePhase{
		kyberv1.MachinePhaseReady,
		kyberv1.MachinePhaseRunning,
		kyberv1.MachinePhaseStopped,
		kyberv1.MachinePhaseStopping,
	} {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			r := &AgentReconciler{
				MachineGetter: &fakeMachineGetter{
					machine: &kyberv1.Machine{
						Status: kyberv1.MachineStatus{Phase: phase},
					},
				},
			}
			agent := &kyberv1.Agent{
				Spec:       kyberv1.AgentSpec{Machine: "node-01"},
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
			}
			if r.isMachinePreempted(ctx, agent, nil) {
				t.Errorf("isMachinePreempted: got true, want false for phase %q", phase)
			}
		})
	}
}

// TestIsMachinePreempted_FailedSpotMachine verifies that a Failed spot machine is classified
// as preempted. Spot machines in Failed state lost their node to GCE preemption but the
// machine controller was already in Failed and didn't re-enter Preempted — the agent should
// wait for a replacement rather than counting the event as a crash.
func TestIsMachinePreempted_FailedSpotMachine(t *testing.T) {
	ctx := context.Background()
	r := &AgentReconciler{
		MachineGetter: &fakeMachineGetter{
			machine: &kyberv1.Machine{
				Spec:   kyberv1.MachineSpec{Spot: true},
				Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseFailed},
			},
		},
	}
	agent := &kyberv1.Agent{
		Spec:       kyberv1.AgentSpec{Machine: "node-01"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
	}
	if !r.isMachinePreempted(ctx, agent, nil) {
		t.Error("isMachinePreempted: got false, want true for Failed spot machine")
	}
}

// TestIsMachinePreempted_FailedNonSpotMachine verifies that a Failed non-spot machine is NOT
// classified as preempted. Non-spot machines don't get preempted by GCE — a Failed state
// means a real error and should count against the agent's retry budget.
func TestIsMachinePreempted_FailedNonSpotMachine(t *testing.T) {
	ctx := context.Background()
	r := &AgentReconciler{
		MachineGetter: &fakeMachineGetter{
			machine: &kyberv1.Machine{
				Spec:   kyberv1.MachineSpec{Spot: false},
				Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseFailed},
			},
		},
	}
	agent := &kyberv1.Agent{
		Spec:       kyberv1.AgentSpec{Machine: "node-01"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
	}
	if r.isMachinePreempted(ctx, agent, nil) {
		t.Error("isMachinePreempted: got true, want false for Failed non-spot machine")
	}
}

// newFakeReconcilerWithNodes builds a minimal AgentReconciler backed by a fake client
// pre-populated with the given Node objects. Used in node-status preemption detection tests.
func newFakeReconcilerWithNodes(t *testing.T, nodes ...*corev1.Node) *AgentReconciler {
	t.Helper()
	scheme := buildTestScheme()
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, n := range nodes {
		builder = builder.WithObjects(n)
	}
	return &AgentReconciler{
		Client: builder.Build(),
	}
}

// notReadyNode returns a Node with the given name whose Ready condition is False.
func notReadyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}
}

// readyNode returns a Node with the given name whose Ready condition is True.
func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

// TestIsMachinePreempted_SpotMachineWithNotReadyNode verifies the node-status early detection
// path: a Running spot machine whose node is NotReady should be classified as preempted even
// though the machine controller hasn't transitioned to Preempted yet.
func TestIsMachinePreempted_SpotMachineWithNotReadyNode(t *testing.T) {
	ctx := context.Background()
	r := newFakeReconcilerWithNodes(t, notReadyNode("gke-node-1"))
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Spec:   kyberv1.MachineSpec{Spot: true},
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}
	agent := &kyberv1.Agent{
		Spec:       kyberv1.AgentSpec{Machine: "node-01"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Status:     kyberv1.AgentStatus{NodeName: "gke-node-1"},
	}
	if !r.isMachinePreempted(ctx, agent, nil) {
		t.Error("isMachinePreempted: got false, want true for Running spot machine with NotReady node")
	}
}

// TestIsMachinePreempted_SpotMachineWithReadyNode verifies that a Running spot machine whose
// node is still Ready is NOT classified as preempted — the node is healthy, so pod death is
// a real crash that should count against the retry budget.
func TestIsMachinePreempted_SpotMachineWithReadyNode(t *testing.T) {
	ctx := context.Background()
	r := newFakeReconcilerWithNodes(t, readyNode("gke-node-1"))
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Spec:   kyberv1.MachineSpec{Spot: true},
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}
	agent := &kyberv1.Agent{
		Spec:       kyberv1.AgentSpec{Machine: "node-01"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Status:     kyberv1.AgentStatus{NodeName: "gke-node-1"},
	}
	if r.isMachinePreempted(ctx, agent, nil) {
		t.Error("isMachinePreempted: got true, want false for Running spot machine with Ready node")
	}
}

// TestIsMachinePreempted_NonSpotMachineWithNotReadyNode verifies that a non-spot machine is NOT
// classified as preempted even when its node is NotReady. Non-spot machines don't get preempted
// by GCE — a NotReady node means a real failure that should count against the retry budget.
func TestIsMachinePreempted_NonSpotMachineWithNotReadyNode(t *testing.T) {
	ctx := context.Background()
	r := newFakeReconcilerWithNodes(t, notReadyNode("gke-node-2"))
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Spec:   kyberv1.MachineSpec{Spot: false},
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}
	agent := &kyberv1.Agent{
		Spec:       kyberv1.AgentSpec{Machine: "node-01"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Status:     kyberv1.AgentStatus{NodeName: "gke-node-2"},
	}
	if r.isMachinePreempted(ctx, agent, nil) {
		t.Error("isMachinePreempted: got true, want false for Running non-spot machine with NotReady node")
	}
}

// TestClassifyEvent_TerminatingPodTreatedAsDead verifies that a Running agent whose pod has
// a DeletionTimestamp (stuck Terminating on a dead node) is treated the same as a nil pod.
// When the machine is preempted, the agent should transition to WaitingForMachine; when the
// machine is not preempted, it should transition to Failed (via EventPodDied).
func TestClassifyEvent_TerminatingPodTreatedAsDead(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()

	t.Run("preempted machine", func(t *testing.T) {
		r := newReconciler(k8sClient, scheme)
		r.MachineGetter = &fakeMachineGetter{
			machine: &kyberv1.Machine{
				Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhasePreempted},
			},
		}

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-terminating-preempted"}}
		if err := k8sClient.Create(context.Background(), ns); err != nil {
			t.Fatalf("creating namespace: %v", err)
		}

		agent := newTestAgent("dave", "test-terminating-preempted")
		if err := k8sClient.Create(context.Background(), agent); err != nil {
			t.Fatalf("creating agent: %v", err)
		}
		agentKey := types.NamespacedName{Name: "dave", Namespace: "test-terminating-preempted"}
		req := ctrl.Request{NamespacedName: agentKey}

		// Bootstrap to Creating.
		reconcileN(t, r, req, 1)

		// Fast-forward to Running.
		updated := getAgent(t, k8sClient, agentKey)
		patch := client.MergeFrom(updated.DeepCopy())
		now := metav1.Now()
		updated.Status.Phase = kyberv1.AgentPhaseRunning
		updated.Status.LastTransition = &now
		updated.Status.StartTime = &now
		if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
			t.Fatalf("patching to Running: %v", err)
		}

		// Mark the pod as Terminating by setting DeletionTimestamp via delete (envtest honours finalizers
		// but since there are none the pod transitions immediately — simulate by injecting a pod with
		// DeletionTimestamp into classifyEvent directly).
		deletionTime := metav1.Now()
		terminatingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              AgentPodName("dave"),
				Namespace:         "test-terminating-preempted",
				DeletionTimestamp: &deletionTime,
				Finalizers:        []string{"test/block-deletion"}, // keeps DeletionTimestamp without actual deletion
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}

		ctx := context.Background()
		agentObj := getAgent(t, k8sClient, agentKey)
		event, err := r.classifyEvent(ctx, agentObj, terminatingPod)
		if err != nil {
			t.Fatalf("classifyEvent: %v", err)
		}
		if event != EventMachineUnavailable {
			t.Errorf("classifyEvent with terminating pod + preempted machine: got %q, want %q", event, EventMachineUnavailable)
		}
	})

	t.Run("non-preempted machine", func(t *testing.T) {
		r := newReconciler(k8sClient, scheme)
		r.MachineGetter = &fakeMachineGetter{
			machine: &kyberv1.Machine{
				Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
			},
		}

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-terminating-nonpreempted"}}
		if err := k8sClient.Create(context.Background(), ns); err != nil {
			t.Fatalf("creating namespace: %v", err)
		}

		agent := newTestAgent("dave", "test-terminating-nonpreempted")
		if err := k8sClient.Create(context.Background(), agent); err != nil {
			t.Fatalf("creating agent: %v", err)
		}
		agentKey := types.NamespacedName{Name: "dave", Namespace: "test-terminating-nonpreempted"}
		req := ctrl.Request{NamespacedName: agentKey}

		reconcileN(t, r, req, 1)

		updated := getAgent(t, k8sClient, agentKey)
		patch := client.MergeFrom(updated.DeepCopy())
		now := metav1.Now()
		updated.Status.Phase = kyberv1.AgentPhaseRunning
		updated.Status.LastTransition = &now
		updated.Status.StartTime = &now
		if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
			t.Fatalf("patching to Running: %v", err)
		}

		// Aged past the 60s recently-deleted guard: a FRESH DeletionTimestamp
		// now means "graceful roll in progress — wait", and only a pod STUCK
		// Terminating (the dead-node case this test documents) still
		// classifies as dead. See reconciler_terminating_pod_test.go.
		deletionTime := metav1.NewTime(time.Now().Add(-2 * time.Minute))
		terminatingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              AgentPodName("dave"),
				Namespace:         "test-terminating-nonpreempted",
				DeletionTimestamp: &deletionTime,
				Finalizers:        []string{"test/block-deletion"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}

		ctx := context.Background()
		agentObj := getAgent(t, k8sClient, agentKey)
		event, err := r.classifyEvent(ctx, agentObj, terminatingPod)
		if err != nil {
			t.Fatalf("classifyEvent: %v", err)
		}
		if event != EventPodDied {
			t.Errorf("classifyEvent with terminating pod + running machine: got %q, want %q", event, EventPodDied)
		}
	})
}

// TestClassifyEvent_RunningNilPod_MachinePreempted verifies that classifyEvent returns
// the provider-neutral MachineUnavailable event (not EventPodDied) when a Running agent's pod is nil and the
// machine is in a preempted phase.
func TestClassifyEvent_RunningNilPod_MachinePreempted(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhasePreempted},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-classify-preempted"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-classify-preempted")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-classify-preempted"}

	// Bootstrap to Creating then manually set Running.
	req := ctrl.Request{NamespacedName: agentKey}
	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Delete the pod to simulate machine preemption (pod is gone).
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-classify-preempted"}
	pod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, pod); err == nil {
		if err := k8sClient.Delete(context.Background(), pod); err != nil && !errors.IsNotFound(err) {
			t.Fatalf("deleting pod: %v", err)
		}
	}

	// Reconcile: Running + nil pod + preempted machine → WaitingForMachine.
	reconcileN(t, r, req, 1)

	finalAgent := getAgent(t, k8sClient, agentKey)
	if finalAgent.Status.Phase != kyberv1.AgentPhaseWaitingForMachine {
		t.Errorf("phase: got %q, want %q", finalAgent.Status.Phase, kyberv1.AgentPhaseWaitingForMachine)
	}
}

// TestClassifyEvent_RunningWithPreemptionAnnotation verifies that classifyEvent returns
// EventPreemptionNotice (and removes the annotation) when a Running agent has the
// kyber.dev/preemption-notice annotation set.
func TestClassifyEvent_RunningWithPreemptionAnnotation(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-classify-preemption-notice"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-classify-preemption-notice")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-classify-preemption-notice"}
	req := ctrl.Request{NamespacedName: agentKey}

	// Bootstrap to Creating.
	reconcileN(t, r, req, 1)

	// Fast-forward to Running.
	updated := getAgent(t, k8sClient, agentKey)
	statusPatch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, statusPatch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Set the preemption-notice annotation.
	agentObj := getAgent(t, k8sClient, agentKey)
	annotPatch := client.MergeFrom(agentObj.DeepCopy())
	if agentObj.Annotations == nil {
		agentObj.Annotations = make(map[string]string)
	}
	agentObj.Annotations["kyber.dev/preemption-notice"] = "true"
	if err := k8sClient.Patch(context.Background(), agentObj, annotPatch); err != nil {
		t.Fatalf("setting preemption-notice annotation: %v", err)
	}

	// Reconcile: Running + preemption-notice annotation → EventPreemptionNotice → Draining.
	reconcileN(t, r, req, 1)

	finalAgent := getAgent(t, k8sClient, agentKey)
	if finalAgent.Status.Phase != kyberv1.AgentPhaseDraining {
		t.Errorf("phase: got %q, want %q", finalAgent.Status.Phase, kyberv1.AgentPhaseDraining)
	}
	// Annotation must be removed.
	if finalAgent.Annotations["kyber.dev/preemption-notice"] != "" {
		t.Error("kyber.dev/preemption-notice annotation was not removed after classifyEvent")
	}
}

// TestReconciler_HandleDeletion_DeletesAgentSecrets verifies that the finalizer deletes
// secrets labeled kyber.io/agent=<name> and does not touch secrets for other agents.
func TestReconciler_HandleDeletion_DeletesAgentSecrets(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-secret-cleanup"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("alice", "test-secret-cleanup")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "alice", Namespace: "test-secret-cleanup"}}
	agentKey := types.NamespacedName{Name: "alice", Namespace: "test-secret-cleanup"}

	// Bootstrap: first reconcile sets up finalizer + PVC + pod, phase → Creating.
	reconcileN(t, r, req, 1)

	created := getAgent(t, k8sClient, agentKey)
	if !containsString(created.Finalizers, AgentFinalizer) {
		t.Fatal("finalizer not set before deletion test")
	}

	// User-secrets Secrets (#75) must be eagerly created by the reconciler on
	// the bootstrap pass so the pod's unconditional mounts resolve.
	for _, name := range []string{UserSecretKVName("alice"), UserSecretFilesName("alice")} {
		got := &corev1.Secret{}
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "test-secret-cleanup"}, got); err != nil {
			t.Fatalf("user-secret %s not created by reconciler: %v", name, err)
		}
		if got.Labels["kyber.io/agent"] != "alice" {
			t.Errorf("user-secret %s label kyber.io/agent: got %q, want %q", name, got.Labels["kyber.io/agent"], "alice")
		}
	}

	// Create alice's secrets (labeled kyber.io/agent=alice).
	secretA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-oauth",
			Namespace: "test-secret-cleanup",
			Labels:    map[string]string{"kyber.io/agent": "alice"},
		},
	}
	secretB := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-telegram",
			Namespace: "test-secret-cleanup",
			Labels:    map[string]string{"kyber.io/agent": "alice"},
		},
	}
	// A secret belonging to a different agent — must NOT be deleted.
	secretC := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bob-oauth",
			Namespace: "test-secret-cleanup",
			Labels:    map[string]string{"kyber.io/agent": "bob"},
		},
	}
	for _, sec := range []*corev1.Secret{secretA, secretB, secretC} {
		if err := k8sClient.Create(context.Background(), sec); err != nil {
			t.Fatalf("creating secret %s: %v", sec.Name, err)
		}
	}

	// Delete the agent CRD (sets DeletionTimestamp; finalizer holds it).
	if err := k8sClient.Delete(context.Background(), created); err != nil {
		t.Fatalf("deleting agent: %v", err)
	}

	// Reconcile enough times to let the finalizer run:
	// pass 1 — deletes pod, requeues
	// pass 2 — pod gone, deletes PVC + secrets, removes finalizer
	// pass 3 — agent gone (IsNotFound path), no-op
	reconcileN(t, r, req, 3)

	ctx := context.Background()
	got := &corev1.Secret{}

	// alice-oauth and alice-telegram must be deleted.
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "alice-oauth", Namespace: "test-secret-cleanup"}, got); !errors.IsNotFound(err) {
		t.Fatalf("alice-oauth should be deleted, got err=%v", err)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "alice-telegram", Namespace: "test-secret-cleanup"}, got); !errors.IsNotFound(err) {
		t.Fatalf("alice-telegram should be deleted, got err=%v", err)
	}

	// User-secrets Secrets (#75) must also be deleted — same label, same finalizer path.
	for _, name := range []string{UserSecretKVName("alice"), UserSecretFilesName("alice")} {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "test-secret-cleanup"}, got); !errors.IsNotFound(err) {
			t.Fatalf("user-secret %s should be deleted, got err=%v", name, err)
		}
	}

	// bob-oauth must still exist.
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "bob-oauth", Namespace: "test-secret-cleanup"}, got); err != nil {
		t.Fatalf("bob-oauth should still exist, got err=%v", err)
	}
}

// TestClassifyEvent_ExitCode2_ReturnsOAuthRefreshFailed verifies that classifyEvent returns
// EventOAuthRefreshFailed (not EventPodDied) when the agent container exits with code 2.
func TestClassifyEvent_ExitCode2_ReturnsOAuthRefreshFailed(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-oauth-exitcode2"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-oauth-exitcode2")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-oauth-exitcode2"}
	req := ctrl.Request{NamespacedName: agentKey}

	// Bootstrap to Creating.
	reconcileN(t, r, req, 1)

	// Fast-forward to Running.
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Build a Failed pod whose agent container exited with code 2.
	exitCode2Pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-oauth-exitcode2",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 2,
							Reason:   "Error",
						},
					},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, exitCode2Pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventOAuthRefreshFailed {
		t.Errorf("classifyEvent with exit-code-2 pod: got %q, want %q", event, EventOAuthRefreshFailed)
	}
}

// TestClassifyEvent_ForceNeedsAuth verifies the operator-forced re-auth gate
// (#395): desiredPhase=NeedsAuth derives EventDesiredNeedsAuth from each
// recoverable phase and derives nothing of the kind from the out-of-scope
// transient/cleanup phases. The guard short-circuits ahead of the pod-state
// logic for the recoverable phases, so a nil pod and a zero-value reconciler
// (no machine, no client) are sufficient — the recoverable-phase allowlist is
// the security-relevant gate and is what this test pins down.
func TestClassifyEvent_ForceNeedsAuth(t *testing.T) {
	r := &AgentReconciler{}
	ctx := context.Background()

	forced := func(phase kyberv1.AgentPhase) *kyberv1.Agent {
		return &kyberv1.Agent{
			Spec:   kyberv1.AgentSpec{DesiredPhase: kyberv1.AgentPhaseNeedsAuth},
			Status: kyberv1.AgentStatus{Phase: phase},
		}
	}

	// Recoverable phases: must derive EventDesiredNeedsAuth.
	allowed := []kyberv1.AgentPhase{
		kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting,
		kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseMemoryExhausted,
		kyberv1.AgentPhaseStopped,
	}
	for _, ph := range allowed {
		event, err := r.classifyEvent(ctx, forced(ph), nil)
		if err != nil {
			t.Errorf("phase %s: unexpected error: %v", ph, err)
			continue
		}
		if event != EventDesiredNeedsAuth {
			t.Errorf("phase %s + desired=NeedsAuth: got %q, want %q", ph, event, EventDesiredNeedsAuth)
		}
	}

	// Out-of-scope transient/cleanup phases: must NOT derive EventDesiredNeedsAuth
	// (desiredPhase=NeedsAuth is ignored — the agent is left untouched).
	outOfScope := []kyberv1.AgentPhase{
		kyberv1.AgentPhaseCreating, kyberv1.AgentPhaseStopping,
		kyberv1.AgentPhaseRestarting, kyberv1.AgentPhaseDraining,
		kyberv1.AgentPhaseWaitingForMachine, kyberv1.AgentPhaseNeedsAuth,
		kyberv1.AgentPhaseDeleted,
	}
	for _, ph := range outOfScope {
		event, err := r.classifyEvent(ctx, forced(ph), nil)
		if err != nil {
			t.Errorf("phase %s: unexpected error: %v", ph, err)
			continue
		}
		if event == EventDesiredNeedsAuth {
			t.Errorf("phase %s + desired=NeedsAuth: derived %q, want it to be ignored (out of scope)", ph, event)
		}
	}
}

// TestClassifyEvent_AuthoritativeStop verifies the authoritative-Stop kill
// switch (#468): desiredPhase=Stopped derives EventDesiredStopped from every
// phase an operator can hit Stop during an incident — crucially Failed and
// MemoryExhausted, where the agent would otherwise auto-restart and silently
// ignore the operator. The centralized block sits ahead of the per-phase and
// pod-state switches, so a nil pod and a zero-value reconciler are sufficient;
// the recoverable-phase allowlist is the security-relevant gate this pins down.
func TestClassifyEvent_AuthoritativeStop(t *testing.T) {
	r := &AgentReconciler{}
	ctx := context.Background()

	stopped := func(phase kyberv1.AgentPhase) *kyberv1.Agent {
		return &kyberv1.Agent{
			Spec:   kyberv1.AgentSpec{DesiredPhase: kyberv1.AgentPhaseStopped},
			Status: kyberv1.AgentStatus{Phase: phase},
		}
	}

	// Honored phases: Stop must derive EventDesiredStopped (the AC's required set
	// plus WaitingForMachine for parity). Notably this includes the crash-loop
	// and machine-recovery phases.
	allowed := []kyberv1.AgentPhase{
		kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting,
		kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseMemoryExhausted,
		kyberv1.AgentPhaseWaitingForMachine,
	}
	for _, ph := range allowed {
		event, err := r.classifyEvent(ctx, stopped(ph), nil)
		if err != nil {
			t.Errorf("phase %s: unexpected error: %v", ph, err)
			continue
		}
		if event != EventDesiredStopped {
			t.Errorf("phase %s + desired=Stopped: got %q, want %q", ph, event, EventDesiredStopped)
		}
	}

	// A Failed agent under the auto-restart budget must NOT auto-restart while
	// Stop is desired — Stop pre-empts EventAutoRestartTriggered (AC: pre-empts
	// auto-restart). This is the central regression the #466 incident exposed.
	failedRestartable := stopped(kyberv1.AgentPhaseFailed)
	failedRestartable.Status.RestartCount = 0 // < maxRestartRetries
	event, err := r.classifyEvent(ctx, failedRestartable, nil)
	if err != nil {
		t.Fatalf("Failed + desired=Stopped + RestartCount<max: unexpected error: %v", err)
	}
	if event == EventAutoRestartTriggered {
		t.Errorf("Failed + desired=Stopped + RestartCount<max: auto-restarted (%q), want Stop to pre-empt", event)
	}
	if event != EventDesiredStopped {
		t.Errorf("Failed + desired=Stopped + RestartCount<max: got %q, want %q", event, EventDesiredStopped)
	}

	// Stays-down fixed point: once Status.Phase==Stopped with desired==Stopped,
	// classifyEvent must derive NO event (no pod recreated on resync) until
	// desired flips to Running. Stopped must therefore be EXCLUDED from the
	// honored allowlist (unlike NeedsAuth) — including it would re-fire every
	// reconcile and break the fixed point.
	stableEvent, err := r.classifyEvent(ctx, stopped(kyberv1.AgentPhaseStopped), nil)
	if err != nil {
		t.Fatalf("Stopped + desired=Stopped: unexpected error: %v", err)
	}
	if stableEvent != "" {
		t.Errorf("Stopped + desired=Stopped: derived %q, want \"\" (stable fixed point, stays down)", stableEvent)
	}

	// Out-of-scope transient/cleanup phases must NOT honor Stop (left untouched).
	outOfScope := []kyberv1.AgentPhase{
		kyberv1.AgentPhaseCreating, kyberv1.AgentPhaseStopping,
		kyberv1.AgentPhaseRestarting, kyberv1.AgentPhaseDraining,
		kyberv1.AgentPhaseNeedsAuth, kyberv1.AgentPhaseDeleted,
	}
	for _, ph := range outOfScope {
		event, err := r.classifyEvent(ctx, stopped(ph), nil)
		if err != nil {
			t.Errorf("phase %s: unexpected error: %v", ph, err)
			continue
		}
		if event == EventDesiredStopped {
			t.Errorf("phase %s + desired=Stopped: derived %q, want it ignored (out of scope)", ph, event)
		}
	}
}

// TestClassifyEvent_ExitCode1_WithPriorExitCode2_ReturnsPodDied verifies that a non-OAuth
// crash (exit code 1) is NOT misclassified as OAuthRefreshFailed just because a PREVIOUS
// crash (in LastTerminationState) was exit code 2. Only the current termination matters.
func TestClassifyEvent_ExitCode1_WithPriorExitCode2_ReturnsPodDied(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-oauth-lasttermination"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-oauth-lasttermination")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-oauth-lasttermination"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Current exit code 1 (generic crash), but a PREVIOUS run exited with 2.
	// Must NOT be classified as OAuthRefreshFailed.
	lastTermPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-oauth-lasttermination",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Reason:   "Error",
						},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 2,
							Reason:   "Error",
						},
					},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, lastTermPod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventPodDied {
		t.Errorf("classifyEvent with exit-code-1 (prior exit-code-2): got %q, want %q", event, EventPodDied)
	}
}

// TestReconciler_StartingPhase_Requeues verifies that an agent in Starting phase
// with a not-ready pod gets requeued (instead of silently dropping), so the startup
// timeout can eventually fire.
func TestReconciler_StartingPhase_Requeues(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-starting-requeue"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-starting-requeue")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-starting-requeue"}
	req := ctrl.Request{NamespacedName: agentKey}

	// Bootstrap to Creating → creates pod.
	reconcileN(t, r, req, 1)

	// Fast-forward to Starting with a recent LastTransition (timeout not yet reached).
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStarting
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Starting: %v", err)
	}

	// Pod exists but is NOT Ready (default state after creation).
	// Reconcile should return a RequeueAfter, not an empty result.
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("Starting phase with not-ready pod: got no requeue, want RequeueAfter=%s", requeueWaiting)
	}
}

// TestClassifyEvent_StartingPodFailed_ReturnsPodDied verifies that a pod entering
// PodFailed during the Starting phase is detected as EventPodDied (not silently
// ignored until the startup timeout fires).
func TestClassifyEvent_StartingPodFailed_ReturnsPodDied(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-starting-podfailed"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-starting-podfailed")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-starting-podfailed"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	// Fast-forward to Starting.
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStarting
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Starting: %v", err)
	}

	// Simulate pod that crashed with exit code 1 during startup.
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-starting-podfailed",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Reason:   "Error",
						},
					},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, failedPod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventPodDied {
		t.Errorf("classifyEvent Starting+PodFailed: got %q, want %q", event, EventPodDied)
	}
}

// TestClassifyEvent_StartingExitCode2_ReturnsOAuthRefreshFailed verifies that a pod
// failing with exit code 2 during Starting is classified as EventOAuthRefreshFailed
// (not EventPodDied), so the agent transitions to NeedsAuth.
func TestClassifyEvent_StartingExitCode2_ReturnsOAuthRefreshFailed(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-starting-oauth"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-starting-oauth")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-starting-oauth"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	// Fast-forward to Starting.
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStarting
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Starting: %v", err)
	}

	// Simulate pod that failed with exit code 2 (OAuth refresh failure) during startup.
	oauthFailPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-starting-oauth",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 2,
							Reason:   "Error",
						},
					},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, oauthFailPod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventOAuthRefreshFailed {
		t.Errorf("classifyEvent Starting+ExitCode2: got %q, want %q", event, EventOAuthRefreshFailed)
	}
}

// TestClassifyEvent_RunningExitCode2_SidecarStillUp_ReturnsOAuthRefreshFailed
// pins #274. Post-#248 the agent pod has a kyber-status-sidecar that stays
// running after the agent container exits, so kubelet keeps pod-level
// Phase=Running. Pre-#274 the Running-case dispatch only entered the
// "treat as dead" branch when pod-level Phase was Failed/Succeeded,
// so an agent-only OAuth-refresh failure (exit 2) silently held the
// Agent at Running and the PWA's Re-authorize button never lit up.
//
// This test fixtures the exact production state observed on chewie
// 2026-05-06: pod.Status.Phase=PodRunning, agent terminated exit 2,
// kyber-status-sidecar still running. Expectation: classifyEvent
// returns EventOAuthRefreshFailed despite pod-level Phase=Running.
func TestClassifyEvent_RunningExitCode2_SidecarStillUp_ReturnsOAuthRefreshFailed(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-running-oauth-sidecar"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-running-oauth-sidecar")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-running-oauth-sidecar"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-running-oauth-sidecar",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, // sidecar keeps pod-level Phase here
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 2,
							Reason:   "Error",
						},
					},
				},
				{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventOAuthRefreshFailed {
		t.Errorf("classifyEvent Running+ExitCode2+sidecar-still-up: got %q, want %q",
			event, EventOAuthRefreshFailed)
	}
}

// TestClassifyEvent_StartingExitCode2_SidecarStillUp_ReturnsOAuthRefreshFailed
// is the Starting-phase analogue of the Running test above. A cold-boot pod
// whose first refresh fails exits 2 within seconds, but the sidecar — being
// independent — keeps pod-level Phase=Running, so the Starting case must
// detect the dead agent container directly (#274). Without the fix, the
// agent stays in Starting until the startup timeout fires, and even then
// classifies as a generic EventPodDied rather than EventOAuthRefreshFailed.
func TestClassifyEvent_StartingExitCode2_SidecarStillUp_ReturnsOAuthRefreshFailed(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-starting-oauth-sidecar"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-starting-oauth-sidecar")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-starting-oauth-sidecar"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStarting
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Starting: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-starting-oauth-sidecar",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 2,
							Reason:   "Error",
						},
					},
				},
				{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventOAuthRefreshFailed {
		t.Errorf("classifyEvent Starting+ExitCode2+sidecar-still-up: got %q, want %q",
			event, EventOAuthRefreshFailed)
	}
}

// TestClassifyEvent_RunningOOMKilled_ReturnsEventOOMKilled pins the
// Running-phase OOM detection from kyber#272. When the agent container's
// State.Terminated.Reason is "OOMKilled" (kubelet's standard tag), the
// classifier must return EventOOMKilled BEFORE falling through to the
// generic EventPodDied → Failed → auto-restart path. Auto-restarting on
// the same too-small memory limit would just crash-loop and hide the
// underlying problem; routing to MemoryExhausted forces the operator to
// address the limit before retrying.
//
// Fixture mirrors the post-#274 multi-container reality: pod-level
// Phase=Running (sidecar still alive), agent container terminated with
// OOMKilled reason.
func TestClassifyEvent_RunningOOMKilled_ReturnsEventOOMKilled(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-running-oom"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-running-oom")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-running-oom"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-running-oom",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, // sidecar keeps pod-level Phase here
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137,
							Reason:   "OOMKilled",
						},
					},
				},
				{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventOOMKilled {
		t.Errorf("classifyEvent Running+OOMKilled+sidecar-still-up: got %q, want %q",
			event, EventOOMKilled)
	}
}

// TestClassifyEvent_StartingOOMKilled_ReturnsEventOOMKilled is the
// Starting-phase analogue. A cold-boot pod whose first claude-code launch
// hits memory pressure may be OOM-killed during startup; that case should
// also surface as MemoryExhausted rather than a generic startup failure.
func TestClassifyEvent_StartingOOMKilled_ReturnsEventOOMKilled(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-starting-oom"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-starting-oom")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-starting-oom"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseStarting
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Starting: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-starting-oom",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137,
							Reason:   "OOMKilled",
						},
					},
				},
				{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventOOMKilled {
		t.Errorf("classifyEvent Starting+OOMKilled+sidecar-still-up: got %q, want %q",
			event, EventOOMKilled)
	}
}

// TestClassifyEvent_RunningExitCode137_NoOOMSignal_AfterWindow_FallsThroughToPodDied
// pins the post-#285 behavior: when kubelet does NOT tag OOMKilled
// (PID 1 was a shepherd, kernel killed a child) AND the sidecar's
// memory_oom signal hasn't arrived, classifyEvent waits up to
// oomDetectionWindow then falls through to EventPodDied. The wait
// gives the sidecar's cgroup-counter signal time to land; after the
// window expires we route the death as a generic crash so the agent
// can auto-restart instead of being held in limbo.
func TestClassifyEvent_RunningExitCode137_NoOOMSignal_AfterWindow_FallsThroughToPodDied(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-running-137-no-oom-after-window"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-running-137-no-oom-after-window")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-running-137-no-oom-after-window"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// chewie 2026-05-06 production fixture: exit 137, reason "Error".
	// FinishedAt set to past the oomDetectionWindow so we're outside the
	// wait branch and into the fall-through.
	pastWindow := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-running-137-no-oom-after-window",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   137,
							Reason:     "Error",
							StartedAt:  metav1.NewTime(time.Now().Add(-3 * time.Minute)),
							FinishedAt: pastWindow,
						},
					},
				},
				{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventPodDied {
		t.Errorf("classifyEvent exit-137-no-OOM-after-window: got %q, want %q",
			event, EventPodDied)
	}
}

// TestClassifyEvent_RunningExitCode137_WithKernelOOMSignal_ReturnsEventOOMKilled
// pins the kyber#285 happy path: agent died exit 137, kubelet didn't tag
// (chewie's exact production case), but the sidecar's memory_oom event
// has populated Status.LastKernelOOMKillAt with a timestamp BETWEEN the
// container's StartedAt and now. Routes to EventOOMKilled →
// AgentPhaseMemoryExhausted, NOT EventPodDied → auto-restart.
func TestClassifyEvent_RunningExitCode137_WithKernelOOMSignal_ReturnsEventOOMKilled(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-running-137-with-kernel-oom"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-running-137-with-kernel-oom")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-running-137-with-kernel-oom"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	startedAt := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	kernelOOMAt := metav1.NewTime(time.Now().Add(-30 * time.Second))

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	updated.Status.LastKernelOOMKillAt = &kernelOOMAt
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running with kernel OOM: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-running-137-with-kernel-oom",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   137,
							Reason:     "Error",
							StartedAt:  startedAt,
							FinishedAt: metav1.Now(),
						},
					},
				},
				{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventOOMKilled {
		t.Errorf("classifyEvent exit-137 + recent kernel OOM: got %q, want %q",
			event, EventOOMKilled)
	}
}

// TestClassifyEvent_RunningExitCode137_NoOOMSignal_WithinWindow_Waits
// pins the wait branch: exit-137 within oomDetectionWindow with no
// kernel-OOM signal yet returns "" (no event) so the controller
// requeues and re-evaluates on the next reconcile (typically
// triggered by the sidecar's pending status patch).
func TestClassifyEvent_RunningExitCode137_NoOOMSignal_WithinWindow_Waits(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-running-137-within-window"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-running-137-within-window")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-running-137-within-window"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// FinishedAt 5s ago — well within the 30s oomDetectionWindow.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-running-137-within-window",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   137,
							Reason:     "Error",
							StartedAt:  metav1.NewTime(time.Now().Add(-1 * time.Minute)),
							FinishedAt: metav1.NewTime(time.Now().Add(-5 * time.Second)),
						},
					},
				},
				{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != "" {
		t.Errorf("classifyEvent exit-137 within window with no kernel-OOM signal: got %q, want \"\" (waiting)",
			event)
	}
}

// TestClassifyEvent_RunningExitCode137_KernelOOMBeforeContainerStart_DoesNotAttribute
// pins the safety check in hasRecentKernelOOMKill: a kernel OOM
// observed BEFORE this container's StartedAt (e.g. from a previous
// container life within the same pod) is NOT attributed to this death.
// Otherwise a fresh non-OOM crash would inherit a stale kernel-OOM
// stamp and route to MemoryExhausted incorrectly.
func TestClassifyEvent_RunningExitCode137_KernelOOMBeforeContainerStart_DoesNotAttribute(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	r.MachineGetter = &fakeMachineGetter{
		machine: &kyberv1.Machine{
			Status: kyberv1.MachineStatus{Phase: kyberv1.MachinePhaseRunning},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-running-137-stale-oom"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("dave", "test-running-137-stale-oom")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-running-137-stale-oom"}
	req := ctrl.Request{NamespacedName: agentKey}

	reconcileN(t, r, req, 1)

	// Stale OOM observed 1h ago, BEFORE this container's recent start.
	staleOOMAt := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	containerStartedAt := metav1.NewTime(time.Now().Add(-1 * time.Minute))

	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	updated.Status.StartTime = &now
	updated.Status.LastKernelOOMKillAt = &staleOOMAt
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running with stale OOM: %v", err)
	}

	// FinishedAt past the wait window so we don't get the requeue branch.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentPodName("dave"),
			Namespace: "test-running-137-stale-oom",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   137,
							Reason:     "Error",
							StartedAt:  containerStartedAt,
							FinishedAt: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
						},
					},
				},
				{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}

	ctx := context.Background()
	agentObj := getAgent(t, k8sClient, agentKey)
	event, err := r.classifyEvent(ctx, agentObj, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventPodDied {
		t.Errorf("classifyEvent exit-137 with stale (pre-start) kernel-OOM: got %q, want %q",
			event, EventPodDied)
	}
}

// TestReconciler_RestartPodDiedRace verifies that when the pod dies while
// desiredPhase=Restarting (race between pod termination and controller processing),
// the agent transitions through Restarting→Starting, NOT through Failed.
// This prevents the confusing "Failed" flash in the PWA during operator-triggered restarts.
func TestReconciler_RestartPodDiedRace(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-restart-race"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	agent := newTestAgent("racer", "test-restart-race")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "racer", Namespace: "test-restart-race"}}
	agentKey := types.NamespacedName{Name: "racer", Namespace: "test-restart-race"}

	// Bootstrap to Creating.
	reconcileN(t, r, req, 1)

	// Fast-forward to Running with a pod.
	updated := getAgent(t, k8sClient, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	if err := k8sClient.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching to Running: %v", err)
	}

	// Set desiredPhase=Restarting AND delete the pod to simulate the race:
	// the pod terminates before the controller sees the Restarting intent.
	agentObj := getAgent(t, k8sClient, agentKey)
	specPatch := client.MergeFrom(agentObj.DeepCopy())
	agentObj.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	if err := k8sClient.Patch(context.Background(), agentObj, specPatch); err != nil {
		t.Fatalf("setting desiredPhase=Restarting: %v", err)
	}

	// Delete the pod to simulate it dying before the controller processes the restart.
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: "racer", Namespace: "test-restart-race"}
	if err := k8sClient.Get(context.Background(), podKey, pod); err == nil {
		if err := k8sClient.Delete(context.Background(), pod); err != nil {
			t.Fatalf("deleting pod: %v", err)
		}
		// Wait for the pod to be fully gone.
		for i := 0; i < 20; i++ {
			if err := k8sClient.Get(context.Background(), podKey, pod); err != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Reconcile: Running + pod gone + desiredPhase=Restarting → should go to Restarting, NOT Failed.
	reconcileN(t, r, req, 1)

	agentAfter := getAgent(t, k8sClient, agentKey)
	if agentAfter.Status.Phase == kyberv1.AgentPhaseFailed {
		t.Error("agent flashed through Failed — the race condition was NOT fixed")
	}
	if agentAfter.Status.Phase != kyberv1.AgentPhaseRestarting {
		t.Errorf("phase after restart race: got %q, want %q",
			agentAfter.Status.Phase, kyberv1.AgentPhaseRestarting)
	}

	// Next reconcile: Restarting + pod gone → Starting (creates new pod).
	reconcileN(t, r, req, 1)

	agentFinal := getAgent(t, k8sClient, agentKey)
	if agentFinal.Status.Phase != kyberv1.AgentPhaseStarting {
		t.Errorf("phase after restart completion: got %q, want %q",
			agentFinal.Status.Phase, kyberv1.AgentPhaseStarting)
	}
}

// TestIsSidecarDrifted_SpecMatch_NoDrift pins the kyber#371 Defect B
// fix (Matt's Option b): isSidecarDrifted now collapses onto
// isSidecarSpecMismatched — pod's status-sidecar spec image equal to
// controller's StatusSidecarImage means no drift. Replaces the prior
// digest-comparison shape that false-positived on multi-arch images.
func TestIsSidecarDrifted_SpecMatch_NoDrift(t *testing.T) {
	const img = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	pod := podWithSidecarSpecImage("agent-alice", img)
	if isSidecarDrifted(pod, img) {
		t.Error("got drifted=true, want false (spec strings equal)")
	}
}

// TestIsSidecarDrifted_SpecMismatch_Drift pins the kyber#299 prod
// scenario under the new spec-string contract: control-plane env
// bumped to v1.3.5 but the pod still carries v1.0.0 in its spec.
// Drift fires.
func TestIsSidecarDrifted_SpecMismatch_Drift(t *testing.T) {
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	ctrlImg := "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	if !isSidecarDrifted(pod, ctrlImg) {
		t.Error("got drifted=false, want true (spec strings differ)")
	}
}

// TestIsSidecarDrifted_NoPod_False — defensive: nil pod returns false.
// Reconcile may run before a pod exists.
func TestIsSidecarDrifted_NoPod_False(t *testing.T) {
	if isSidecarDrifted(nil, "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5") {
		t.Error("nil pod must not register drift")
	}
}

// TestIsSidecarDrifted_MultiArchIndexPin_NotDrifted is kyber#371 AC #4:
// when the controller pins a multi-arch index digest and the pod's
// status-sidecar spec image carries the same string (both derive from
// the same KYBER_STATUS_SIDECAR_IMAGE env), drift is false — even
// though kubelet's containerStatuses[*].imageID would report a
// different per-platform manifest digest. Reproduces the GHCR
// ghcr.io/matty-v/kyber-status-sidecar:latest case observed in
// production (index sha256:1649… vs amd64 manifest sha256:01e656…).
func TestIsSidecarDrifted_MultiArchIndexPin_NotDrifted(t *testing.T) {
	const indexPin = "ghcr.io/matty-v/kyber-status-sidecar:latest@sha256:1649000000000000000000000000000000000000000000000000000000000000"
	pod := podWithSidecarSpecImage("agent-alice", indexPin)
	if isSidecarDrifted(pod, indexPin) {
		t.Error("multi-arch index-pin spec equality must not register drift (was kyber#371 Defect B false-positive)")
	}
}

// TestIsSidecarDrifted_IncompatibleDigestKinds_NotDrifted is kyber#371
// AC #5: incompatible digest KINDS (index vs platform manifest) used
// to make isSidecarDrifted always-true. Under the spec-equality
// contract both ends derive their image string from the same env, so
// the practical case is "equal strings → no drift". When the strings
// genuinely differ (e.g., bump landed) it correctly registers as
// drift; the "kinds don't match" failure mode no longer exists at the
// source because we never extract digests.
func TestIsSidecarDrifted_IncompatibleDigestKinds_NotDrifted(t *testing.T) {
	// Practical case: both ends got the index pin from the same env.
	const indexPin = "ghcr.io/matty-v/kyber-status-sidecar:latest@sha256:1649000000000000000000000000000000000000000000000000000000000000"
	pod := podWithSidecarSpecImage("agent-alice", indexPin)
	if isSidecarDrifted(pod, indexPin) {
		t.Error("equal spec strings (index pin) must not register drift")
	}
}

// TestReconcileSidecarDriftCondition_NotRunning_RemovesCondition pins
// that the condition is cleared when the agent isn't in Running phase.
// Other phases (Starting/Stopping/etc.) the drift signal isn't
// meaningful — the controller is mid-roll, the pod isn't stable, or
// the pod doesn't exist yet. Leaving a stale True condition would
// confuse the dirty surface.
func TestReconcileSidecarDriftCondition_NotRunning_RemovesCondition(t *testing.T) {
	r := &AgentReconciler{
		StatusSidecarImage: "ghcr.io/matty-v/kyber-status-sidecar:latest@sha256:current",
	}
	agent := &kyberv1.Agent{
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseStopping,
			Conditions: []metav1.Condition{
				{
					Type:               kyberv1.AgentConditionSidecarOutOfDate,
					Status:             metav1.ConditionTrue,
					Reason:             "Stale",
					Message:            "left over from when the agent was Running",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
	changed := r.reconcileSidecarDriftCondition(agent, &corev1.Pod{})
	if !changed {
		t.Error("expected condition state to change (remove)")
	}
	for _, c := range agent.Status.Conditions {
		if c.Type == kyberv1.AgentConditionSidecarOutOfDate {
			t.Errorf("condition still present after remove: %+v", c)
		}
	}
}

// TestReconcileSidecarDriftCondition_RunningWithDrift_SetsTrue pins
// the kyber#299 happy path under the kyber#371 Defect B spec-equality
// contract: phase=Running, pod's status-sidecar spec image differs
// from the controller's current image → condition is set to True with
// PodPredatesSidecarUpdate reason.
func TestReconcileSidecarDriftCondition_RunningWithDrift_SetsTrue(t *testing.T) {
	r := &AgentReconciler{
		StatusSidecarImage: "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5",
	}
	agent := &kyberv1.Agent{
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseRunning,
		},
	}
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	changed := r.reconcileSidecarDriftCondition(agent, pod)
	if !changed {
		t.Error("expected condition state to change")
	}
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionSidecarOutOfDate)
	if cond == nil {
		t.Fatal("SidecarOutOfDate condition not set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition.Status: got %q, want True", cond.Status)
	}
	if cond.Reason != "PodPredatesSidecarUpdate" {
		t.Errorf("condition.Reason: got %q, want PodPredatesSidecarUpdate", cond.Reason)
	}
}

// TestReconcileSidecarDriftCondition_RunningCurrent_SetsFalse pins
// the inverse under the kyber#371 spec-equality contract: phase=Running,
// pod's status-sidecar spec image matches controller's current image →
// condition is set to False with Current reason. Avoids the false-
// positive case where a missing condition would defer to "drifted"
// downstream.
func TestReconcileSidecarDriftCondition_RunningCurrent_SetsFalse(t *testing.T) {
	const img = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	r := &AgentReconciler{
		StatusSidecarImage: img,
	}
	agent := &kyberv1.Agent{
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseRunning,
		},
	}
	pod := podWithSidecarSpecImage("agent-alice", img)
	r.reconcileSidecarDriftCondition(agent, pod)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionSidecarOutOfDate)
	if cond == nil {
		t.Fatal("SidecarOutOfDate condition not set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("condition.Status: got %q, want False", cond.Status)
	}
	if cond.Reason != "Current" {
		t.Errorf("condition.Reason: got %q, want Current", cond.Reason)
	}
}

// --- kyber#299 Option B (auto-roll) -----------------------------------

// driftedAgent builds an Agent in Phase=Running with a SidecarOutOfDate
// condition transitioned `condAge` ago and the given activity state.
// Used to set up the various auto-roll gating tests below.
func driftedAgent(t *testing.T, condStatus metav1.ConditionStatus, condAge time.Duration, activityState string) *kyberv1.Agent {
	t.Helper()
	a := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"},
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseRunning,
			Conditions: []metav1.Condition{
				{
					Type:               kyberv1.AgentConditionSidecarOutOfDate,
					Status:             condStatus,
					Reason:             "PodPredatesSidecarUpdate",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-condAge)),
				},
			},
		},
	}
	if activityState != "" {
		a.Status.Activity = &kyberv1.ActivityStatus{State: activityState}
	}
	return a
}

func driftedPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber",
			Labels:    map[string]string{"kyber.io/agent": "alice"},
		},
	}
}

func newAutoRollReconciler(t *testing.T, enabled bool, objs ...client.Object) *AgentReconciler {
	t.Helper()
	objs = append(objs, driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "idle"))
	c := fake.NewClientBuilder().
		WithScheme(schedulingTestScheme(t)).
		WithObjects(objs...).
		Build()
	return &AgentReconciler{
		Client:                 c,
		Recorder:               record.NewFakeRecorder(16),
		StatusSidecarImage:     "ghcr.io/matty-v/kyber-status-sidecar:stable@sha256:newdigest",
		SidecarAutoRollEnabled: enabled,
	}
}

func TestMaybeAutoRollSidecar_Disabled_NoOp(t *testing.T) {
	pod := driftedPod("agent-alice")
	agent := driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "idle")
	r := newAutoRollReconciler(t, false, pod)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when SidecarAutoRollEnabled=false")
	}
}

func TestMaybeAutoRollSidecar_NilPod_NoOp(t *testing.T) {
	agent := driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "idle")
	r := newAutoRollReconciler(t, true)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when pod is nil")
	}
}

func TestMaybeAutoRollSidecar_PodAlreadyDeleting_NoOp(t *testing.T) {
	now := metav1.Now()
	pod := driftedPod("agent-alice")
	pod.DeletionTimestamp = &now
	agent := driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "idle")
	r := newAutoRollReconciler(t, true)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when this pod is already mid-deletion")
	}
}

func TestMaybeAutoRollSidecar_NotRunning_NoOp(t *testing.T) {
	pod := driftedPod("agent-alice")
	agent := driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "idle")
	agent.Status.Phase = kyberv1.AgentPhaseStarting
	r := newAutoRollReconciler(t, true, pod)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when phase is not Running")
	}
}

func TestMaybeAutoRollSidecar_ConditionFalse_NoOp(t *testing.T) {
	pod := driftedPod("agent-alice")
	agent := driftedAgent(t, metav1.ConditionFalse, 10*time.Minute, "idle")
	r := newAutoRollReconciler(t, true, pod)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when SidecarOutOfDate is False")
	}
}

func TestMaybeAutoRollSidecar_NotStableYet_NoOp(t *testing.T) {
	pod := driftedPod("agent-alice")
	// Condition flipped 1 minute ago — under the 5-minute default.
	agent := driftedAgent(t, metav1.ConditionTrue, 1*time.Minute, "idle")
	r := newAutoRollReconciler(t, true, pod)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when condition has not been True long enough")
	}
}

func TestMaybeAutoRollSidecar_NotIdle_NoOp(t *testing.T) {
	pod := driftedPod("agent-alice")
	agent := driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "working")
	r := newAutoRollReconciler(t, true, pod)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when agent is working")
	}
	// Sanity: pod should still be present.
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, &corev1.Pod{}); err != nil {
		t.Fatalf("pod should still exist: %v", err)
	}
}

func TestMaybeAutoRollSidecar_NoActivityField_NoOp(t *testing.T) {
	pod := driftedPod("agent-alice")
	// activityState=="" leaves Status.Activity unset.
	agent := driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "")
	r := newAutoRollReconciler(t, true, pod)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when Activity is nil (state unknown)")
	}
}

func TestMaybeAutoRollSidecar_AnotherPodDeleting_NoOp(t *testing.T) {
	pod := driftedPod("agent-alice")
	now := metav1.Now()
	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "agent-bob",
			Namespace:         "kyber",
			Labels:            map[string]string{"kyber.io/agent": "bob"},
			DeletionTimestamp: &now,
			Finalizers:        []string{"kyber.io/agent-cleanup"},
		},
	}
	agent := driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "idle")
	r := newAutoRollReconciler(t, true, pod, otherPod)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no roll when another agent pod is mid-deletion (concurrency gate)")
	}
}

func TestMaybeAutoRollSidecar_AllGatesPass_RollsPod(t *testing.T) {
	pod := driftedPod("agent-alice")
	agent := driftedAgent(t, metav1.ConditionTrue, 10*time.Minute, "idle")
	r := newAutoRollReconciler(t, true, pod)
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("expected roll when all gates pass")
	}
	// The pod stays up until the state machine observes the durable restart
	// request, preventing the deletion from being misclassified as a crash.
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, &corev1.Pod{}); err != nil {
		t.Fatalf("pod should remain until Restarting is recorded; got err=%v", err)
	}
	gotAgent := &kyberv1.Agent{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: agent.Namespace, Name: agent.Name}, gotAgent); err != nil {
		t.Fatalf("fetching restart request: %v", err)
	}
	if gotAgent.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("desiredPhase=%q, want Restarting", gotAgent.Spec.DesiredPhase)
	}
	// FakeRecorder should have buffered the event.
	rec, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatalf("Recorder is not *FakeRecorder")
	}
	select {
	case msg := <-rec.Events:
		if !strings.Contains(msg, "SidecarOutOfDateAutoRoll") {
			t.Errorf("event missing reason: %q", msg)
		}
	default:
		t.Errorf("expected an event to be recorded; channel was empty")
	}
}

func TestMaybeAutoRollSidecar_CustomMinStable_HonorsConfig(t *testing.T) {
	pod := driftedPod("agent-alice")
	// Condition flipped 30s ago. Default 5m would block; custom 10s passes.
	agent := driftedAgent(t, metav1.ConditionTrue, 30*time.Second, "idle")
	r := newAutoRollReconciler(t, true, pod)
	r.SidecarAutoRollMinStable = 10 * time.Second
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("expected roll once custom min-stable threshold is met")
	}
}

func TestCountAgentPodsBeingDeleted(t *testing.T) {
	now := metav1.Now()
	live := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent-alice", Namespace: "kyber",
			Labels: map[string]string{"kyber.io/agent": "alice"},
		},
	}
	deleting := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent-bob", Namespace: "kyber",
			Labels:            map[string]string{"kyber.io/agent": "bob"},
			DeletionTimestamp: &now,
			Finalizers:        []string{"kyber.io/agent-cleanup"},
		},
	}
	// A non-agent pod that happens to be deleting — must NOT count.
	nonAgent := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "control-plane-xyz", Namespace: "kyber",
			DeletionTimestamp: &now,
			Finalizers:        []string{"kubernetes"},
		},
	}
	r := newAutoRollReconciler(t, true, live, deleting, nonAgent)
	got, err := r.countAgentPodsBeingDeleted(context.Background(), "kyber")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1", got)
	}
}

func TestRequestIntentionalRestart_NonRunningDoesNotReserveRollout(t *testing.T) {
	agent := idleAgent("alice", "kyber")
	agent.Status.Phase = kyberv1.AgentPhaseMemoryExhausted
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	r := &AgentReconciler{Client: fake.NewClientBuilder().
		WithScheme(schedulingTestScheme(t)).
		WithObjects(agent).
		Build()}

	requested, err := r.requestIntentionalRestart(context.Background(), agent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requested {
		t.Fatal("a non-Running agent must not reserve an automatic restart")
	}
	got := &kyberv1.Agent{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), got); err != nil {
		t.Fatalf("fetching agent: %v", err)
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		t.Fatalf("desiredPhase=%q, want operator intent Running unchanged", got.Spec.DesiredPhase)
	}
	inflight, err := r.countAgentPodsBeingDeleted(context.Background(), agent.Namespace)
	if err != nil {
		t.Fatalf("counting rollouts: %v", err)
	}
	if inflight != 0 {
		t.Fatalf("inflight=%d, want 0; declined restart must not consume rollout budget", inflight)
	}
}

// ---- kyber#358 tag-level sidecar convergence ----

// podWithSidecarSpecImage returns a fake agent pod whose status-sidecar
// container's spec image is set to the given value. Used by the kyber#358
// convergence tests.
func podWithSidecarSpecImage(name, sidecarImage string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber",
			Labels:    map[string]string{"kyber.io/agent": strings.TrimPrefix(name, "agent-")},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent", Image: "ghcr.io/matty-v/agent-claude-code:v1"},
				{Name: "kyber-status-sidecar", Image: sidecarImage},
			},
		},
	}
}

// newConvergeReconciler builds a reconciler with the given controller image
// and seed objects, ready to exercise convergeSidecarImage via the fake client.
func newConvergeReconciler(t *testing.T, controllerImage string, objs ...client.Object) *AgentReconciler {
	t.Helper()
	seen := map[string]bool{}
	for _, obj := range objs {
		if a, ok := obj.(*kyberv1.Agent); ok {
			seen[a.Name] = true
		}
	}
	for _, obj := range objs {
		if p, ok := obj.(*corev1.Pod); ok {
			name := p.Labels["kyber.io/agent"]
			if name != "" && !seen[name] {
				objs = append(objs, idleAgent(name, p.Namespace))
				seen[name] = true
			}
		}
	}
	c := fake.NewClientBuilder().
		WithScheme(schedulingTestScheme(t)).
		WithObjects(objs...).
		Build()
	return &AgentReconciler{
		Client:             c,
		Recorder:           record.NewFakeRecorder(16),
		StatusSidecarImage: controllerImage,
	}
}

// TestIsSidecarSpecMismatched_TagMatch_NoMismatch — common case for tag-pinned
// installs: chart was helm-upgraded, control-plane env matches what the pod
// already has.
func TestIsSidecarSpecMismatched_TagMatch_NoMismatch(t *testing.T) {
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0")
	if isSidecarSpecMismatched(pod, "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0") {
		t.Error("got mismatched=true, want false (tags identical)")
	}
}

// TestIsSidecarSpecMismatched_TagMismatch_True — the live prod scenario:
// pod spec still on v1.0.0, controller env on v1.3.0. Must fire.
func TestIsSidecarSpecMismatched_TagMismatch_True(t *testing.T) {
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	ctrlImg := "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0"
	if !isSidecarSpecMismatched(pod, ctrlImg) {
		t.Error("got mismatched=false, want true (different tags — the live prod case)")
	}
}

// TestIsSidecarSpecMismatched_NilPod_False — defensive: nil pod returns false.
// Reconcile can run before a pod exists; convergence is a no-op then.
func TestIsSidecarSpecMismatched_NilPod_False(t *testing.T) {
	if isSidecarSpecMismatched(nil, "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0") {
		t.Error("nil pod must not register mismatch")
	}
}

// TestIsSidecarSpecMismatched_EmptyControllerImage_False — defensive: never
// delete a pod over an unset KYBER_STATUS_SIDECAR_IMAGE env. Safer to leave the
// pod alone than to wipe the fleet on a misconfiguration.
func TestIsSidecarSpecMismatched_EmptyControllerImage_False(t *testing.T) {
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	if isSidecarSpecMismatched(pod, "") {
		t.Error("empty controller image must not register mismatch")
	}
}

// TestIsSidecarSpecMismatched_SidecarContainerAbsent_False — pod exists but
// has no kyber-status-sidecar container in spec (could happen mid-rebuild or
// for a legacy pod). No diff signal.
func TestIsSidecarSpecMismatched_SidecarContainerAbsent_False(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "ghcr.io/matty-v/agent-claude-code:v1"}},
		},
	}
	if isSidecarSpecMismatched(pod, "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0") {
		t.Error("pod without sidecar container must not register mismatch")
	}
}

// TestConvergeSidecarImage_NoMismatch_NoOp — happy path: tags match, no delete.
func TestConvergeSidecarImage_NoMismatch_NoOp(t *testing.T) {
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0")
	r := newConvergeReconciler(t, "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0", pod)
	rolled, err := r.convergeSidecarImage(context.Background(),
		&kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no convergence when sidecar tag already matches")
	}
	// Pod must still exist.
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Errorf("pod must not be deleted on match; got error fetching: %v", err)
	}
}

// TestConvergeSidecarImage_Mismatch_RequestsRestartAndRequeues — kyber#358
// live prod case (chart bump v1.0.0 → v1.3.0) under kyber#371's
// hardened gates: agent is idle, image is the first canary attempt
// in this controller process. Convergence delete fires; the canary
// FSM marks the image as in-flight.
func TestConvergeSidecarImage_Mismatch_RequestsRestartAndRequeues(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, pod)
	agent := idleAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("expected convergence to fire on tag mismatch (v1.0.0 → v1.3.0)")
	}
	// The state machine owns deletion after it records Restarting.
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Errorf("pod must remain until restart intent is observed; got err=%v", err)
	}
	gotAgent := &kyberv1.Agent{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "kyber", Name: "alice"}, gotAgent); err != nil {
		t.Fatalf("fetching restart request: %v", err)
	}
	if gotAgent.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("desiredPhase=%q, want Restarting", gotAgent.Spec.DesiredPhase)
	}
	if _, inFlight := r.sidecarCanaryInFlight(target); !inFlight {
		t.Error("expected canary to be marked in-flight after first eligible delete")
	}
}

// TestConvergeSidecarImage_PodAlreadyDeleting_NoOp — never double-delete.
// A pod with DeletionTimestamp is already on its way out.
func TestConvergeSidecarImage_PodAlreadyDeleting_NoOp(t *testing.T) {
	now := metav1.Now()
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"kyber.io/agent-cleanup"}
	r := newConvergeReconciler(t, "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0", pod)
	rolled, err := r.convergeSidecarImage(context.Background(),
		&kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no convergence when pod is already deleting")
	}
}

// TestConvergeSidecarImage_NilPod_NoOp — Reconcile may run before any pod
// exists (e.g. before the state machine creates one). No diff, no delete.
func TestConvergeSidecarImage_NilPod_NoOp(t *testing.T) {
	r := newConvergeReconciler(t, "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0")
	rolled, err := r.convergeSidecarImage(context.Background(),
		&kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("nil pod must never trigger a convergence delete")
	}
}

// TestConvergeSidecarImage_EmptyControllerEnv_NoOp — defensive: if
// KYBER_STATUS_SIDECAR_IMAGE is unset for any reason, never delete a pod
// just because its sidecar image doesn't equal the empty string. Safer
// to leave the fleet alone than to wipe it on a misconfiguration.
func TestConvergeSidecarImage_EmptyControllerEnv_NoOp(t *testing.T) {
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, "", pod)
	rolled, err := r.convergeSidecarImage(context.Background(),
		&kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("empty controller image must never trigger a delete")
	}
}

// idleAgent builds an Agent with Activity.State=idle in the kyber#371
// idle-gate satisfying shape. Used by convergence tests that need to
// pass the gate.
func idleAgent(name, namespace string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: kyberv1.AgentStatus{
			Phase:    kyberv1.AgentPhaseRunning,
			Activity: &kyberv1.ActivityStatus{State: "idle"},
		},
	}
}

// workingAgent builds an Agent with Activity.State=working — the case
// the kyber#371 idle gate exists to protect.
func workingAgent(name, namespace string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: kyberv1.AgentStatus{
			Phase:    kyberv1.AgentPhaseRunning,
			Activity: &kyberv1.ActivityStatus{State: "working"},
		},
	}
}

// readySidecarPod returns a pod whose status-sidecar container is
// reported Ready and State.Running — the verification-trigger positive
// signal. specImage and the controller image must agree at the call
// site for the trigger to mark the image verified.
func readySidecarPod(name, sidecarImage string) *corev1.Pod {
	p := podWithSidecarSpecImage(name, sidecarImage)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name:  "kyber-status-sidecar",
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		},
	}
	return p
}

// drainEvents returns the buffered FakeRecorder events as strings —
// non-blocking; reads until the channel is empty. Used by tests that
// assert which events were emitted (or not).
func drainEvents(t *testing.T, rec *record.FakeRecorder) []string {
	t.Helper()
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// --- kyber#371 Defect A — hardened convergeSidecarImage gates -----------

// TestConvergeSidecarImage_UnpullableImage_NoWorkingAgentDelete is
// kyber#371 AC #1 — the R2-D2 regression. When the controller pins an
// unpullable image (a 404 tag, garbage digest, etc.) AND the agent's
// runtime activity is Working, convergeSidecarImage must defer; no
// pod deletion may fire.
func TestConvergeSidecarImage_UnpullableImage_NoWorkingAgentDelete(t *testing.T) {
	const bad = "ghcr.io/matty-v/kyber-status-sidecar:0.1.0" // unpullable in prod
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5")
	r := newConvergeReconciler(t, bad, pod)
	agent := workingAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected convergence to defer on Working agent under unpullable image (R2-D2 regression)")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Errorf("pod must not be deleted; got err=%v", err)
	}
	// Even the canary FSM must not start while the agent is Working —
	// the idle gate runs before the pullability gate.
	if _, inFlight := r.sidecarCanaryInFlight(bad); inFlight {
		t.Error("canary must not be armed when the idle gate blocks")
	}
}

// TestConvergeSidecarImage_IdleGate_DefersOnWorking is kyber#371 AC #2:
// even with a known-good (verified) image, a Working agent's pod is
// never deleted. Mirrors maybeAutoRollSidecarForDrift's idle gate
// exactly.
func TestConvergeSidecarImage_IdleGate_DefersOnWorking(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, pod)
	r.markSidecarImageVerified(target) // canary already passed in this process
	agent := workingAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("idle gate must block delete on Working agent even with verified image")
	}
}

// TestConvergeSidecarImage_IdleGate_DefersOnNilActivity covers the
// kyber#371 conservative posture: missing Activity is treated as
// "unknown working" — never delete a pod we can't characterize. Same
// shape as maybeAutoRollSidecarForDrift's nil-Activity gate.
func TestConvergeSidecarImage_IdleGate_DefersOnNilActivity(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, pod)
	r.markSidecarImageVerified(target)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"}}
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("nil Activity must block delete (same conservative posture as 5c)")
	}
}

// TestConvergeSidecarImage_ConcurrencyCap_DefersWhenAnotherDeleting
// is kyber#371 AC #3: when any agent pod in the namespace is mid-
// deletion, this convergence defers. 5c and 5d cooperatively share
// sidecarAutoRollDefaultMaxConcurrent=1.
func TestConvergeSidecarImage_ConcurrencyCap_DefersWhenAnotherDeleting(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	now := metav1.Now()
	deletingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent-bob", Namespace: "kyber",
			Labels:            map[string]string{"kyber.io/agent": "bob"},
			DeletionTimestamp: &now,
			Finalizers:        []string{"kyber.io/agent-cleanup"},
		},
	}
	livePod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, livePod, deletingPod)
	r.markSidecarImageVerified(target) // image is fine; only the cap blocks
	agent := idleAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, livePod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("expected no delete while another agent pod is mid-deletion (cooperative cap with 5c)")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(livePod), &corev1.Pod{}); err != nil {
		t.Errorf("live pod must not be deleted under cap; got err=%v", err)
	}
}

// TestConvergeSidecarImage_ValidBump_ConvergesIdleAgentsInOneCycle is
// kyber#371 AC #6: a *valid* sidecar-image bump still converges idle
// agent pods within one reconcile cycle once the image is verified
// (or, for the first hit, once it becomes the canary). Preserves the
// kyber#358 design intent.
func TestConvergeSidecarImage_ValidBump_ConvergesIdleAgentsInOneCycle(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, pod)
	r.markSidecarImageVerified(target) // operator verified the bump; steady-state path
	agent := idleAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("expected valid (verified) bump to converge idle agent in one cycle")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Errorf("pod must remain until the state machine records Restarting; got err=%v", err)
	}
	rec, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatal("Recorder is not *FakeRecorder")
	}
	events := drainEvents(t, rec)
	var sawConverge bool
	for _, e := range events {
		if strings.Contains(e, "SidecarImageConverge") {
			sawConverge = true
		}
	}
	if !sawConverge {
		t.Errorf("expected SidecarImageConverge event; got %v", events)
	}
}

// TestMaybeAutoRollSidecarForDrift_MultiArchAutoRollEnabled_NoFalseRoll
// is kyber#371 AC #7: with KYBER_SIDECAR_AUTO_ROLL=true and a multi-
// arch sidecar image correctly pinned at both ends, the dormant
// kyber#299 false-positive (index-vs-platform digest comparison) does
// not fire. The SidecarOutOfDate condition must be False and no pod
// roll fires across the stability window.
func TestMaybeAutoRollSidecarForDrift_MultiArchAutoRollEnabled_NoFalseRoll(t *testing.T) {
	const indexPin = "ghcr.io/matty-v/kyber-status-sidecar:latest@sha256:1649000000000000000000000000000000000000000000000000000000000000"
	pod := podWithSidecarSpecImage("agent-alice", indexPin)
	c := fake.NewClientBuilder().
		WithScheme(schedulingTestScheme(t)).
		WithObjects(pod).
		Build()
	r := &AgentReconciler{
		Client:                 c,
		Recorder:               record.NewFakeRecorder(16),
		StatusSidecarImage:     indexPin,
		SidecarAutoRollEnabled: true,
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber"},
		Status:     kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning},
	}
	// The drift condition path: spec equality → False, not True.
	r.reconcileSidecarDriftCondition(agent, pod)
	cond := meta.FindStatusCondition(agent.Status.Conditions, kyberv1.AgentConditionSidecarOutOfDate)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("multi-arch index pin spec-equality must yield False condition; got %+v", cond)
	}
	// Pretend the stability window has long since passed; the False
	// condition must keep auto-roll silent.
	cond.LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Minute))
	agent.Status.Activity = &kyberv1.ActivityStatus{State: "idle"}
	rolled, err := r.maybeAutoRollSidecarForDrift(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("multi-arch index pin must not trigger auto-roll (kyber#371 AC #7: dormant bug stays dormant)")
	}
}

// --- kyber#371 canary FSM unit tests -----------------------------------

// TestSidecarCanary_FirstAttempt_BootstrapsCanary: convergence on a
// brand-new image with no canary state arms the canary clock and
// deletes the first eligible pod (that pod IS the canary).
func TestSidecarCanary_FirstAttempt_BootstrapsCanary(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, pod)
	agent := idleAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("first eligible delete must fire to bootstrap the canary")
	}
	if _, inFlight := r.sidecarCanaryInFlight(target); !inFlight {
		t.Error("canary must be armed after the first delete")
	}
}

func TestSidecarCanary_ConflictingOperatorIntentDoesNotArm(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	agent := idleAgent("alice", "kyber")
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	r := newConvergeReconciler(t, target, agent, pod)

	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("automatic convergence must yield to operator intent")
	}
	if _, inFlight := r.sidecarCanaryInFlight(target); inFlight {
		t.Fatal("declined restart must not arm a phantom canary")
	}
}

// TestSidecarCanary_InFlight_DefersFurtherDeletes: while the canary
// window is open, subsequent eligible pods on the same image are NOT
// deleted — only one pod at risk per (controller process, bad image).
func TestSidecarCanary_InFlight_DefersFurtherDeletes(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	alice := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	bob := podWithSidecarSpecImage("agent-bob", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, alice, bob)
	r.markSidecarCanaryStarted(target) // canary fired in an earlier reconcile
	agent := idleAgent("bob", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, bob)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("second pod must defer while canary window is open")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(bob), &corev1.Pod{}); err != nil {
		t.Errorf("bob's pod must still exist; got err=%v", err)
	}
}

// TestSidecarCanary_WindowElapsedWithoutReady_MarksFailed: when the
// canary window elapses without a Ready observation, the image is
// marked failed and a SidecarImageRollHeld event is emitted.
func TestSidecarCanary_WindowElapsedWithoutReady_MarksFailed(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:bad"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, pod)
	r.SidecarImageCanaryWindow = 50 * time.Millisecond
	r.markSidecarCanaryStarted(target)
	time.Sleep(75 * time.Millisecond)
	agent := idleAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("convergence must defer after window elapsed")
	}
	if !r.sidecarImageFailedCanary(target) {
		t.Error("canary must be marked failed after window elapsed without Ready")
	}
	rec := r.Recorder.(*record.FakeRecorder)
	events := drainEvents(t, rec)
	var sawHeld bool
	for _, e := range events {
		if strings.Contains(e, "SidecarImageRollHeld") {
			sawHeld = true
		}
	}
	if !sawHeld {
		t.Errorf("expected SidecarImageRollHeld event; got %v", events)
	}
}

// TestSidecarCanary_FailedImage_NeverRolls: a previously-failed image
// blocks all further convergence deletes — operator hot-fix (which
// produces a new image string) or controller restart is the recovery
// path.
func TestSidecarCanary_FailedImage_NeverRolls(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:bad"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, pod)
	r.markSidecarCanaryFailed(target)
	agent := idleAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("failed-canary image must never trigger a delete")
	}
	rec := r.Recorder.(*record.FakeRecorder)
	events := drainEvents(t, rec)
	var sawHeld bool
	for _, e := range events {
		if strings.Contains(e, "SidecarImageRollHeld") {
			sawHeld = true
		}
	}
	if !sawHeld {
		t.Errorf("expected SidecarImageRollHeld event on failed image; got %v", events)
	}
}

// TestSidecarCanary_VerifiedImage_NormalConvergence: once an image is
// verified (kubelet flipped Ready on some pod), convergence proceeds
// against any eligible pod with no canary bookkeeping.
func TestSidecarCanary_VerifiedImage_NormalConvergence(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	pod := podWithSidecarSpecImage("agent-alice", "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0")
	r := newConvergeReconciler(t, target, pod)
	r.markSidecarImageVerified(target)
	agent := idleAgent("alice", "kyber")
	rolled, err := r.convergeSidecarImage(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("verified image must converge at steady state")
	}
}

// TestSidecarCanary_VerificationClearsPriorCanaryState: a verification
// event after a canary was already in flight cleans up the in-flight
// entry so subsequent reconciles take the steady-state path.
func TestSidecarCanary_VerificationClearsPriorCanaryState(t *testing.T) {
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	r := &AgentReconciler{StatusSidecarImage: target}
	r.markSidecarCanaryStarted(target)
	if _, inFlight := r.sidecarCanaryInFlight(target); !inFlight {
		t.Fatal("canary not armed after start")
	}
	r.markSidecarImageVerified(target)
	if _, inFlight := r.sidecarCanaryInFlight(target); inFlight {
		t.Error("verification must clear in-flight canary entry")
	}
	if !r.sidecarImageWasVerified(target) {
		t.Error("verification must persist as verified")
	}
}

// TestSidecarCanary_EmptyImage_Inert: defensive — empty image strings
// must not key any FSM state.
func TestSidecarCanary_EmptyImage_Inert(t *testing.T) {
	r := &AgentReconciler{}
	r.markSidecarImageVerified("")
	r.markSidecarCanaryStarted("")
	r.markSidecarCanaryFailed("")
	if r.sidecarImageWasVerified("") {
		t.Error("empty image must not register as verified")
	}
	if _, inFlight := r.sidecarCanaryInFlight(""); inFlight {
		t.Error("empty image must not register as in-flight")
	}
	if r.sidecarImageFailedCanary("") {
		t.Error("empty image must not register as failed")
	}
}

// --- kyber#371 verification trigger ------------------------------------

// TestIsSidecarReady covers the kubelet-side observed-evidence signal:
// only Ready=true AND State.Running=non-nil counts as proof the image
// pulled. Missing sidecar container, non-running state, or Ready=false
// all return false.
func TestIsSidecarReady(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{name: "nil pod", pod: nil, want: false},
		{
			name: "no sidecar container in status",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", Ready: true}},
			}},
			want: false,
		},
		{
			name: "sidecar present, Ready=false",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "kyber-status-sidecar",
					Ready: false,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			}},
			want: false,
		},
		{
			name: "sidecar Ready but not Running (terminated)",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
				}},
			}},
			want: false,
		},
		{
			name: "sidecar Ready and Running",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "kyber-status-sidecar",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			}},
			want: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSidecarReady(tt.pod); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReconcile_VerifiesImageOnReadyPodMatchingControllerImage covers
// the kyber#371 verification trigger: when a reconcile sees a pod
// running the controller's current StatusSidecarImage AND the sidecar
// container is Ready, the image is marked verified for the rest of
// this controller process. Exercised end-to-end by running one
// reconcile against an envtest fixture; the verification map is a
// private field so we assert via sidecarImageWasVerified.
func TestReconcile_VerifiesImageOnReadyPodMatchingControllerImage(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	const target = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.5"
	r.StatusSidecarImage = target

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-verification-trigger"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("alice", "test-verification-trigger")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "alice", Namespace: "test-verification-trigger"}}
	reconcileN(t, r, req, 1)

	podKey := types.NamespacedName{Name: AgentPodName("alice"), Namespace: "test-verification-trigger"}
	bootstrapped := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, bootstrapped); err != nil {
		t.Fatalf("bootstrap pod: %v", err)
	}
	// envtest doesn't run containers — patch the pod's status to look
	// like kubelet has reported Ready on the sidecar.
	patch := client.MergeFrom(bootstrapped.DeepCopy())
	bootstrapped.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "kyber-status-sidecar",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if err := k8sClient.Status().Patch(context.Background(), bootstrapped, patch); err != nil {
		t.Fatalf("patching pod status: %v", err)
	}

	// One more reconcile picks up the Ready signal and trips the
	// verification trigger.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !r.sidecarImageWasVerified(target) {
		t.Errorf("expected image %q to be verified after a Ready observation", target)
	}
}

// TestReconciler_SidecarConvergence_UsesIntentionalRestart is the AC#7 + AC#8
// end-to-end integration test on envtest: bootstrap an Agent + Pod on
// sidecar image A, change the controller's StatusSidecarImage to B, run
// reconciles through the rollout. It pins the production regression: Kyber's
// own convergence deletion must go Running → Restarting → Starting, never
// Running → Failed, and must not consume restartCount or crash backoff.
func TestReconciler_SidecarConvergence_UsesIntentionalRestart(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	const oldImage = "ghcr.io/matty-v/kyber-status-sidecar:v1.0.0"
	const newImage = "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0"
	r.StatusSidecarImage = oldImage

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-sidecar-converge"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("alice", "test-sidecar-converge")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "alice", Namespace: "test-sidecar-converge"}}
	podKey := types.NamespacedName{Name: AgentPodName("alice"), Namespace: "test-sidecar-converge"}

	// Bootstrap: first reconcile creates PVC + pod on oldImage.
	reconcileN(t, r, req, 1)
	originalPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, originalPod); err != nil {
		t.Fatalf("getting bootstrap pod: %v", err)
	}
	if got := extractSidecarSpecImage(originalPod); got != oldImage {
		t.Fatalf("bootstrap pod sidecar image: got %q, want %q (createPod did not honor the old env)", got, oldImage)
	}
	originalUID := originalPod.UID

	current := &kyberv1.Agent{}
	agentKey := types.NamespacedName{Name: "alice", Namespace: "test-sidecar-converge"}
	if err := k8sClient.Get(context.Background(), agentKey, current); err != nil {
		t.Fatalf("re-fetching agent: %v", err)
	}
	phasePatch := client.MergeFrom(current.DeepCopy())
	current.Status.Phase = kyberv1.AgentPhaseRunning
	current.Status.RestartCount = 2
	current.Status.Activity = &kyberv1.ActivityStatus{State: "idle"}
	if err := k8sClient.Status().Patch(context.Background(), current, phasePatch); err != nil {
		t.Fatalf("patching running agent status: %v", err)
	}

	// Simulate the control-plane pod rolling with a new env (chart upgrade).
	// In production this happens when ArgoCD applies the new image.statusSidecar.tag
	// and Kubernetes restarts the control-plane Deployment — the new controller
	// reads the new env at startup. In the test we mutate the field directly.
	r.StatusSidecarImage = newImage

	// Satisfy the kyber#371 idle gate — convergeSidecarImage now refuses
	// to interrupt an agent the runtime reports as Working (or unknown).
	// In production the sidecar's activity probe populates this; envtest
	// has no real pod so we patch the status directly.
	// First pass records intent but deliberately leaves the pod running.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("convergence reconcile: %v", err)
	}
	assertAgent := func(want kyberv1.AgentPhase) *kyberv1.Agent {
		t.Helper()
		got := &kyberv1.Agent{}
		if err := k8sClient.Get(context.Background(), agentKey, got); err != nil {
			t.Fatalf("getting agent: %v", err)
		}
		if got.Status.Phase != want {
			t.Fatalf("phase=%q, want %q", got.Status.Phase, want)
		}
		if got.Status.RestartCount != 2 {
			t.Fatalf("restartCount=%d, want unchanged 2", got.Status.RestartCount)
		}
		return got
	}
	got := assertAgent(kyberv1.AgentPhaseRunning)
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("desiredPhase=%q, want Restarting", got.Spec.DesiredPhase)
	}
	stillRunning := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, stillRunning); err != nil || stillRunning.UID != originalUID {
		t.Fatalf("old pod must remain before restart transition: uid=%s err=%v", stillRunning.UID, err)
	}

	// Second pass records Restarting and deletes via the state machine.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("restart transition reconcile: %v", err)
	}
	assertAgent(kyberv1.AgentPhaseRestarting)

	// Third pass observes the deletion and recreates immediately, without the
	// Failed-phase 10/30/90-second throttle.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("recreate reconcile: %v", err)
	}
	assertAgent(kyberv1.AgentPhaseStarting)
	replacement := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, replacement); err != nil {
		t.Fatalf("getting replacement pod: %v", err)
	}
	if got := extractSidecarSpecImage(replacement); got != newImage {
		t.Fatalf("replacement sidecar image=%q, want %q", got, newImage)
	}
}

// TestDesiredPhaseEnum_AcceptsEveryAPISettablePhase pins the wire contract
// between the API setter and the Agent CRD schema: every phase that
// setAgentDesiredPhase can write must be accepted by the API server.
//
// This exists because that contract was silently broken. #395 added
// POST /agents/{name}/force-needs-auth, which routes to
// setAgentDesiredPhase(..., AgentPhaseNeedsAuth), and the controller gate
// (classifyEvent's EventDesiredNeedsAuth arm) handled it correctly — but
// NeedsAuth was never added to the desiredPhase enum. The API server rejected
// the write as invalid, setAgentDesiredPhase mapped the failure to a blanket
// 500 "failed to update agent", and the endpoint could never once have
// succeeded. Reproduced live in production: force-needs-auth
// returned 500 while restart (a permitted enum value) returned 200 against the
// same agent seconds later, isolating the value as the only variable.
//
// Why the existing coverage did not catch it: TestClassifyEvent_ForceNeedsAuth
// builds an Agent literal in memory and calls classifyEvent on a zero-value
// reconciler with no client, and the recovery-gate tests use a fake client.
// Neither applies the CRD's OpenAPI schema, so the controller logic was proven
// while the write path was never exercised. This test drives the write through
// envtest against the real generated CRD, which is the only place the enum is
// enforced.
//
// Assert on the whole set rather than NeedsAuth alone: the failure mode is a
// new lifecycle verb being added to the setter without a matching enum entry,
// and enumerating every settable phase is what makes the next one fail here
// instead of in production.
func TestDesiredPhaseEnum_AcceptsEveryAPISettablePhase(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	ctx := context.Background()

	// Mirrors the action switch in pkg/api/routes_agents.go: start, stop,
	// restart, force-needs-auth. Keep in lockstep with that switch.
	settable := []struct {
		action string
		phase  kyberv1.AgentPhase
	}{
		{"start", kyberv1.AgentPhaseRunning},
		{"stop", kyberv1.AgentPhaseStopped},
		{"restart", kyberv1.AgentPhaseRestarting},
		{"force-needs-auth", kyberv1.AgentPhaseNeedsAuth},
	}

	for _, tc := range settable {
		t.Run(tc.action, func(t *testing.T) {
			name := "enum-" + strings.ToLower(string(tc.phase))
			agent := newTestAgent(name, "default")
			if err := k8sClient.Create(ctx, agent); err != nil {
				t.Fatalf("creating agent: %v", err)
			}
			defer k8sClient.Delete(ctx, agent) //nolint:errcheck

			// The same write setAgentDesiredPhase performs.
			patch := client.MergeFrom(agent.DeepCopy())
			agent.Spec.DesiredPhase = tc.phase
			if err := k8sClient.Patch(ctx, agent, patch); err != nil {
				t.Fatalf("POST /agents/{name}/%s writes desiredPhase=%s, which the CRD rejected: %v\n"+
					"Add %s to the +kubebuilder:validation:Enum marker on AgentSpec.DesiredPhase and re-run controller-gen.",
					tc.action, tc.phase, err, tc.phase)
			}

			// Read back through the API server — a patch that silently dropped
			// the field would otherwise pass.
			got := getAgent(t, k8sClient, types.NamespacedName{Name: name, Namespace: "default"})
			if got.Spec.DesiredPhase != tc.phase {
				t.Fatalf("desiredPhase round-trip: got %q, want %q", got.Spec.DesiredPhase, tc.phase)
			}
		})
	}
}
