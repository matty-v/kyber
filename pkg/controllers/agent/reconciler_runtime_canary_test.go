package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Staggered/canary runtime-image roll (kyber#529). The #523/#527 runtime-image
// drift roll fired unconditionally per Running agent, so a fleet-wide
// KYBER_AGENT_RUNTIME_IMAGE bump rolled every agent at once — a bad digest
// fleet-wided into ImagePullBackOff. shouldRollRuntimeImage lifts the kyber#371
// sidecar canary + concurrency machinery (now the image-agnostic
// imageCanaryTracker) onto the runtime roll: bounded waves via a shared
// cluster-wide delete budget, canary-gated so the rest of the fleet is held
// until the first roll proves the image pullable. The ONE intentional
// divergence from convergeSidecarImage is the absence of an idle gate (AC#4 —
// single-agent envs must keep #523/#527's immediate roll). These tests cover
// the gate FSM (unit), the no-idle-gate guarantee, the tracker-isolation that
// makes Option B safe, and the two AC envtests (bounded waves; bad digest
// contained to the canary).

// --- helpers -----------------------------------------------------------------

// podWithAgentImageNS returns a namespaced, agent-labeled pod carrying a single
// agent container on the given image. The namespace + kyber.io/agent label are
// what countAgentPodsBeingDeleted lists on, so the concurrency-cap tests need
// them (podWithAgentSpecImage from the #527 file omits both).
func podWithAgentImageNS(name, agentImage string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber",
			Labels:    map[string]string{"kyber.io/agent": strings.TrimPrefix(name, "agent-")},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: AgentContainerName, Image: agentImage}},
		},
	}
}

// newRuntimeRollReconciler builds a reconciler over a fake client seeded with
// objs, ready to exercise shouldRollRuntimeImage directly (the gate takes the
// desired image as a parameter, so no adapter registry is needed).
func newRuntimeRollReconciler(t *testing.T, objs ...client.Object) *AgentReconciler {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(schedulingTestScheme(t)).
		WithObjects(objs...).
		Build()
	return &AgentReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(16),
	}
}

// --- gate FSM unit tests (mirror the kyber#371 sidecar canary unit tests) ----

// TestShouldRollRuntimeImage_NotDrifted_NoRoll: a pod already on the desired
// image is not rolled, and no canary is armed (the steady-state half).
func TestShouldRollRuntimeImage_NotDrifted_NoRoll(t *testing.T) {
	pod := podWithAgentImageNS("agent-dave", driftDesiredImage)
	r := newRuntimeRollReconciler(t, pod)
	roll, err := r.shouldRollRuntimeImage(context.Background(), idleAgent("dave", "kyber"), pod, driftDesiredImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if roll {
		t.Fatal("a pod already on the desired image must not roll")
	}
	if _, inFlight := r.runtimeCanary.canaryInFlight(driftDesiredImage); inFlight {
		t.Error("no canary may be armed when there is no drift")
	}
}

// TestShouldRollRuntimeImage_FirstAttempt_ArmsCanaryAndAllows: the first
// drifted agent for an unseen image becomes the canary — it is allowed to roll
// and the canary clock is armed.
func TestShouldRollRuntimeImage_FirstAttempt_ArmsCanaryAndAllows(t *testing.T) {
	pod := podWithAgentImageNS("agent-dave", driftOldImage)
	r := newRuntimeRollReconciler(t, pod)
	roll, err := r.shouldRollRuntimeImage(context.Background(), idleAgent("dave", "kyber"), pod, driftDesiredImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !roll {
		t.Fatal("the first drifted agent must be allowed to roll (it is the canary)")
	}
	if _, inFlight := r.runtimeCanary.canaryInFlight(driftDesiredImage); !inFlight {
		t.Error("canary clock must be armed after the first allowed roll")
	}
}

// TestShouldRollRuntimeImage_CanaryInFlight_HoldsRest: while the canary window
// is open, subsequent drifted agents on the same image are held — only one
// agent at risk per (controller process, image).
func TestShouldRollRuntimeImage_CanaryInFlight_HoldsRest(t *testing.T) {
	alice := podWithAgentImageNS("agent-alice", driftOldImage)
	bob := podWithAgentImageNS("agent-bob", driftOldImage)
	r := newRuntimeRollReconciler(t, alice, bob)
	r.runtimeCanary.markCanaryStarted(driftDesiredImage) // canary fired earlier
	roll, err := r.shouldRollRuntimeImage(context.Background(), idleAgent("bob", "kyber"), bob, driftDesiredImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if roll {
		t.Fatal("a second agent must be held while the canary window is open")
	}
}

// TestShouldRollRuntimeImage_ConcurrencyCap_DefersWhenAnotherDeleting: when any
// agent pod in the namespace is mid-deletion, the gate defers — even for a
// verified image. This is the shared cluster-wide budget with 5c/5d.
func TestShouldRollRuntimeImage_ConcurrencyCap_DefersWhenAnotherDeleting(t *testing.T) {
	now := metav1.Now()
	deletingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent-bob", Namespace: "kyber",
			Labels:            map[string]string{"kyber.io/agent": "bob"},
			DeletionTimestamp: &now,
			Finalizers:        []string{"kyber.io/agent-cleanup"},
		},
	}
	alice := podWithAgentImageNS("agent-alice", driftOldImage)
	r := newRuntimeRollReconciler(t, alice, deletingPod)
	r.runtimeCanary.markVerified(driftDesiredImage) // image is fine; only the cap blocks
	roll, err := r.shouldRollRuntimeImage(context.Background(), idleAgent("alice", "kyber"), alice, driftDesiredImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if roll {
		t.Fatal("expected hold while another agent pod is mid-deletion (shared budget with 5c/5d)")
	}
}

// TestShouldRollRuntimeImage_WindowElapsed_MarksFailedAndHolds: when the canary
// window elapses without a Ready observation, the image is marked failed and a
// RuntimeImageRollHeld event is emitted.
func TestShouldRollRuntimeImage_WindowElapsed_MarksFailedAndHolds(t *testing.T) {
	pod := podWithAgentImageNS("agent-dave", driftOldImage)
	r := newRuntimeRollReconciler(t, pod)
	r.RuntimeImageCanaryWindow = 50 * time.Millisecond
	r.runtimeCanary.markCanaryStarted(driftDesiredImage)
	time.Sleep(75 * time.Millisecond)
	roll, err := r.shouldRollRuntimeImage(context.Background(), idleAgent("dave", "kyber"), pod, driftDesiredImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if roll {
		t.Fatal("must hold after the canary window elapses without a Ready pod")
	}
	if !r.runtimeCanary.failedCanary(driftDesiredImage) {
		t.Error("canary must be marked failed after the window elapses without Ready")
	}
	if !sawEvent(t, r, "RuntimeImageRollHeld") {
		t.Error("expected a RuntimeImageRollHeld event when the canary window elapses")
	}
}

// TestShouldRollRuntimeImage_FailedImage_NeverRolls: a previously-failed image
// holds every agent and emits a held event — operator hot-fix (new image
// string) or controller restart is the recovery path.
func TestShouldRollRuntimeImage_FailedImage_NeverRolls(t *testing.T) {
	pod := podWithAgentImageNS("agent-dave", driftOldImage)
	r := newRuntimeRollReconciler(t, pod)
	r.runtimeCanary.markCanaryFailed(driftDesiredImage)
	roll, err := r.shouldRollRuntimeImage(context.Background(), idleAgent("dave", "kyber"), pod, driftDesiredImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if roll {
		t.Fatal("a failed-canary image must never roll an agent")
	}
	if !sawEvent(t, r, "RuntimeImageRollHeld") {
		t.Error("expected a RuntimeImageRollHeld event on a failed image")
	}
}

// TestShouldRollRuntimeImage_VerifiedImage_SteadyStateAllows: once the image is
// verified (a pod was observed Ready on it), the gate allows any drifted agent
// to roll subject only to the concurrency cap — the steady-state wave.
func TestShouldRollRuntimeImage_VerifiedImage_SteadyStateAllows(t *testing.T) {
	pod := podWithAgentImageNS("agent-dave", driftOldImage)
	r := newRuntimeRollReconciler(t, pod)
	r.runtimeCanary.markVerified(driftDesiredImage)
	roll, err := r.shouldRollRuntimeImage(context.Background(), idleAgent("dave", "kyber"), pod, driftDesiredImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !roll {
		t.Fatal("a verified image must roll at steady state")
	}
}

// TestShouldRollRuntimeImage_EmptyDesiredImage_NoRollNoList: an empty desired
// image (misconfig) is caught by the drift check before any client List — never
// roll a live agent onto an empty ref (kyber#360 Cause D), and no canary armed.
func TestShouldRollRuntimeImage_EmptyDesiredImage_NoRollNoList(t *testing.T) {
	pod := podWithAgentImageNS("agent-dave", driftOldImage)
	r := newRuntimeRollReconciler(t, pod)
	roll, err := r.shouldRollRuntimeImage(context.Background(), idleAgent("dave", "kyber"), pod, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if roll {
		t.Fatal("an empty desired image must never roll")
	}
}

// TestShouldRollRuntimeImage_NoIdleGate_WorkingAgentStillRolls is the AC#4
// guarantee: unlike convergeSidecarImage, the runtime roll has NO idle gate. A
// Working agent whose pod is drifted still rolls (preserving #523/#527's
// single-agent behavior, which never consulted Activity). This is the central
// divergence from the sidecar path and the reason the gate is a distinct method
// rather than a reuse of convergeSidecarImage's gate sequence verbatim.
func TestShouldRollRuntimeImage_NoIdleGate_WorkingAgentStillRolls(t *testing.T) {
	pod := podWithAgentImageNS("agent-dave", driftOldImage)
	r := newRuntimeRollReconciler(t, pod)
	working := workingAgent("dave", "kyber") // Activity.State == "working"
	roll, err := r.shouldRollRuntimeImage(context.Background(), working, pod, driftDesiredImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !roll {
		t.Fatal("runtime roll must NOT idle-gate: a Working agent still rolls (AC#4, preserves #523/#527)")
	}
}

// --- isAgentReady (the canary's positive signal) -----------------------------

// TestIsAgentReady mirrors TestIsSidecarReady on the agent container: only
// Ready=true AND State.Running=non-nil counts as proof the agent image pulled.
func TestIsAgentReady(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{name: "nil pod", pod: nil, want: false},
		{
			name: "no agent container in status",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{Name: "kyber-status-sidecar", Ready: true}},
			}},
			want: false,
		},
		{
			name: "agent ready and running",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  AgentContainerName,
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			}},
			want: true,
		},
		{
			name: "agent ready but not running (waiting)",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  AgentContainerName,
					Ready: true,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
				}},
			}},
			want: false,
		},
		{
			name: "agent running but not ready",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  AgentContainerName,
					Ready: false,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAgentReady(tc.pod); got != tc.want {
				t.Errorf("isAgentReady = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- tracker isolation (why Option B is safe) --------------------------------

// TestImageCanaryTracker_NamespaceIsolation pins the property that motivated
// Obi-wan's Option B: the sidecar and runtime canaries are SEPARATE trackers,
// so a Ready observation on the sidecar image can never falsely verify a
// runtime image that happens to share the same reference string, and vice
// versa.
func TestImageCanaryTracker_NamespaceIsolation(t *testing.T) {
	const shared = "ghcr.io/matty-v/some-image:latest@sha256:abc"
	r := &AgentReconciler{}
	r.sidecarCanary.markVerified(shared)
	if r.runtimeCanary.wasVerified(shared) {
		t.Error("a sidecar verification must not leak into the runtime canary")
	}
	r.runtimeCanary.markCanaryFailed(shared)
	if r.sidecarCanary.failedCanary(shared) {
		t.Error("a runtime canary failure must not leak into the sidecar canary")
	}
	if !r.sidecarCanary.wasVerified(shared) {
		t.Error("sidecar canary must still report its own verification")
	}
	if !r.runtimeCanary.failedCanary(shared) {
		t.Error("runtime canary must still report its own failure")
	}
}

// --- envtest: verification trigger -------------------------------------------

// TestReconcile_VerifiesRuntimeImageOnReadyPod is the kyber#529 mirror of
// TestReconcile_VerifiesImageOnReadyPodMatchingControllerImage: when the live
// pod's agent container is Ready on the controller's desired runtime image, the
// reconcile-top verification trigger (5a.1) marks the runtime image verified —
// the positive signal that releases the rest of the fleet to roll.
func TestReconcile_VerifiesRuntimeImageOnReadyPod(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	desiredImage := r.AdapterRegistry["stub"].Image()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-rt-verify"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("alice", "test-rt-verify")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "alice", Namespace: "test-rt-verify"}}
	reconcileN(t, r, req, 1)

	podKey := types.NamespacedName{Name: AgentPodName("alice"), Namespace: "test-rt-verify"}
	bootstrapped := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, bootstrapped); err != nil {
		t.Fatalf("bootstrap pod: %v", err)
	}
	// envtest runs no kubelet — patch the pod status to look like kubelet has
	// reported the agent container Ready on the desired image.
	patch := client.MergeFrom(bootstrapped.DeepCopy())
	bootstrapped.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  AgentContainerName,
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if err := k8sClient.Status().Patch(context.Background(), bootstrapped, patch); err != nil {
		t.Fatalf("patching pod status: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !r.runtimeCanary.wasVerified(desiredImage) {
		t.Errorf("expected runtime image %q to be verified after a Ready agent observation", desiredImage)
	}
}

// --- envtest: AC#3 — bad digest contained to the canary ----------------------

// TestReconciler_RuntimeImageRoll_BadDigestContainedToCanary is AC#3: a
// fleet-wide bump to a bad/unpullable digest must leave the fleet MOSTLY Running
// on the old image — only the canary (the first agent reconciled) is rolled; the
// rest stay Running on the old image because the canary never reaches Ready
// (never verifies), so the canary-in-flight gate holds them. This is the
// thundering-herd containment the issue exists to deliver.
func TestReconciler_RuntimeImageRoll_BadDigestContainedToCanary(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	desiredImage := r.AdapterRegistry["stub"].Image() // the "new" image; never goes Ready in envtest → models a bad pull

	const nsName = "test-rt-baddigest"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	names := []string{"alice", "bob", "carol"}
	for _, n := range names {
		bringAgentToRunningOnOldImage(t, k8sClient, r, nsName, n, driftOldImage)
	}

	// Reconcile alice FIRST — she becomes the canary and rolls.
	aliceReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "alice", Namespace: nsName}}
	reconcileN(t, r, aliceReq, 1)
	alice := getAgent(t, k8sClient, types.NamespacedName{Name: "alice", Namespace: nsName})
	if alice.Status.Phase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("canary (alice) phase: got %q, want %q", alice.Status.Phase, kyberv1.AgentPhaseRestarting)
	}

	// Reconcile bob and carol — the canary is in flight and never Ready, so they
	// must be HELD on the old image.
	for _, n := range []string{"bob", "carol"} {
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: n, Namespace: nsName}}
		reconcileN(t, r, req, 1)
		ag := getAgent(t, k8sClient, types.NamespacedName{Name: n, Namespace: nsName})
		if ag.Status.Phase != kyberv1.AgentPhaseRunning {
			t.Errorf("%s phase: got %q, want %q (held on old image while canary in flight)", n, ag.Status.Phase, kyberv1.AgentPhaseRunning)
		}
		podKey := types.NamespacedName{Name: AgentPodName(n), Namespace: nsName}
		pod := &corev1.Pod{}
		if err := k8sClient.Get(context.Background(), podKey, pod); err != nil {
			t.Fatalf("getting %s pod: %v", n, err)
		}
		if got := extractAgentSpecImage(pod); got != driftOldImage {
			t.Errorf("%s pod image: got %q, want old image %q (must not have been rolled)", n, got, driftOldImage)
		}
	}
	_ = desiredImage
}

// --- envtest: AC#1 — bounded waves (shared concurrency budget) ---------------

// TestReconciler_RuntimeImageRoll_FleetWideRollsInBoundedWaves is AC#1: a
// fleet-wide change rolls Running agents in bounded waves — at most
// runtimeImageRollDefaultMaxConcurrent (=1) concurrent — with the rest staying
// Running on the old image until a slot frees. The image is pre-verified so the
// canary gate is satisfied and the CONCURRENCY cap is the thing under test: with
// one agent pod stuck mid-deletion (a wave-1 roll in flight), the others hold;
// once that slot frees, the next agent rolls.
func TestReconciler_RuntimeImageRoll_FleetWideRollsInBoundedWaves(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)
	desiredImage := r.AdapterRegistry["stub"].Image()
	// Pre-verify the desired image: isolate the concurrency cap from the canary.
	r.runtimeCanary.markVerified(desiredImage)

	const nsName = "test-rt-waves"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	for _, n := range []string{"alice", "bob"} {
		bringAgentToRunningOnOldImage(t, k8sClient, r, nsName, n, driftOldImage)
	}

	// Simulate a wave-1 roll already in flight: alice's pod is stuck Terminating
	// (a finalizer keeps it around with a DeletionTimestamp, exactly what
	// countAgentPodsBeingDeleted counts).
	aliceKey := types.NamespacedName{Name: AgentPodName("alice"), Namespace: nsName}
	alicePod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), aliceKey, alicePod); err != nil {
		t.Fatalf("getting alice pod: %v", err)
	}
	alicePod.Finalizers = append(alicePod.Finalizers, "kyber.io/test-hold")
	if err := k8sClient.Update(context.Background(), alicePod); err != nil {
		t.Fatalf("adding finalizer to alice pod: %v", err)
	}
	if err := k8sClient.Delete(context.Background(), alicePod); err != nil {
		t.Fatalf("deleting alice pod (stuck-terminating): %v", err)
	}

	// With one delete in flight, bob must hold on the old image.
	bobReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "bob", Namespace: nsName}}
	reconcileN(t, r, bobReq, 1)
	bob := getAgent(t, k8sClient, types.NamespacedName{Name: "bob", Namespace: nsName})
	if bob.Status.Phase != kyberv1.AgentPhaseRunning {
		t.Fatalf("bob phase under concurrency cap: got %q, want %q (held until a slot frees)", bob.Status.Phase, kyberv1.AgentPhaseRunning)
	}

	// Free the slot: drop alice's finalizer so the delete completes (inflight→0).
	alicePod = &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), aliceKey, alicePod); err != nil {
		t.Fatalf("re-getting alice pod: %v", err)
	}
	alicePod.Finalizers = nil
	if err := k8sClient.Update(context.Background(), alicePod); err != nil {
		t.Fatalf("removing finalizer from alice pod: %v", err)
	}

	// Now bob's wave can proceed.
	reconcileN(t, r, bobReq, 1)
	bob = getAgent(t, k8sClient, types.NamespacedName{Name: "bob", Namespace: nsName})
	if bob.Status.Phase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("bob phase after slot freed: got %q, want %q (next wave rolls)", bob.Status.Phase, kyberv1.AgentPhaseRestarting)
	}
}

// bringAgentToRunningOnOldImage creates an agent, bootstraps its pod via one
// reconcile, patches it to Running, and mutates the live pod's agent container
// onto oldImage so it reads as drifted from the controller's desired image.
// Shared by the AC#1/AC#3 multi-agent envtests.
func bringAgentToRunningOnOldImage(t *testing.T, c client.Client, r *AgentReconciler, ns, name, oldImage string) {
	t.Helper()
	agent := newTestAgent(name, ns)
	if err := c.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent %s: %v", name, err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	reconcileN(t, r, req, 1)

	agentKey := types.NamespacedName{Name: name, Namespace: ns}
	updated := getAgent(t, c, agentKey)
	patch := client.MergeFrom(updated.DeepCopy())
	now := metav1.Now()
	updated.Status.Phase = kyberv1.AgentPhaseRunning
	updated.Status.LastTransition = &now
	if err := c.Status().Patch(context.Background(), updated, patch); err != nil {
		t.Fatalf("patching %s to Running: %v", name, err)
	}

	podKey := types.NamespacedName{Name: AgentPodName(name), Namespace: ns}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), podKey, pod); err != nil {
		t.Fatalf("getting %s pod: %v", name, err)
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == AgentContainerName {
			pod.Spec.Containers[i].Image = oldImage
		}
	}
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("drifting %s pod image: %v", name, err)
	}
}

// sawEvent reports whether the reconciler's FakeRecorder emitted an event whose
// text contains substr.
func sawEvent(t *testing.T, r *AgentReconciler, substr string) bool {
	t.Helper()
	rec, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatal("Recorder is not *FakeRecorder")
	}
	for _, e := range drainEvents(t, rec) {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}
