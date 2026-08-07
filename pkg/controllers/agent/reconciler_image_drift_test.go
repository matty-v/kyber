package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
)

// Runtime-image-drift roll (#523). A steady Running agent whose live pod is
// running a runtime image different from the controller's currently-desired
// image (adapter.Image()) is rolled onto the new image via EventDesiredRestarting
// — the same transition rollAgentForUserSecret (#515/#517) and operator restarts
// use. Mirrors the sibling status-sidecar spec-image drift detector
// (isSidecarSpecMismatched). These tests cover the AC: helper correctness, the
// classifyEvent trigger + Running-only phase guard + fail-safe guards, and the
// envtest one-roll convergence (the core roll-loop risk).

const (
	// driftDesiredImage is the controller's desired runtime image — a full
	// repo:tag@digest ref, the shape production pins via an image-sync workflow.
	driftDesiredImage = "ghcr.io/matty-v/agent-claude-code:latest@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	// driftOldImage is the same repo:tag with a different digest — the production
	// digest-bump case. A bare tag-to-tag compare would miss this; the full-ref
	// compare must catch it.
	driftOldImage = "ghcr.io/matty-v/agent-claude-code:latest@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// podWithAgentSpecImage returns a pod carrying a single agent container with the
// given spec image. Mirrors podWithSidecarSpecImage.
func podWithAgentSpecImage(name, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: AgentContainerName, Image: image}},
		},
	}
}

// stubAdapterWithImage builds a minimal stub adapter whose Image() returns img.
func stubAdapterWithImage(img string) pkgruntimes.Adapter {
	return pkgruntimes.NewStubAdapter(
		img,
		[]string{"/usr/local/bin/start-claude.sh"},
		nil, nil, nil, nil, 30,
		"/persist/session-brief.json",
		"/persist/session-state.json",
		"CLAUDE_MODEL",
	)
}

// reconcilerWithRuntimeImage builds a reconciler whose "stub" runtime resolves
// to an adapter with the given desired image, backed by an empty fake client.
// Since kyber#529 the drifted-Running path runs through shouldRollRuntimeImage,
// whose concurrency gate calls countAgentPodsBeingDeleted (a client List) — so
// these classifyEvent drift tests need a client. An empty fake client reports
// zero in-flight deletions, so a lone drifted agent is the canary and rolls,
// preserving the #527 single-agent expectations.
func reconcilerWithRuntimeImage(img string) *AgentReconciler {
	c := fake.NewClientBuilder().WithScheme(buildTestScheme()).Build()
	return &AgentReconciler{
		Client:          c,
		Recorder:        record.NewFakeRecorder(16),
		AdapterRegistry: map[string]pkgruntimes.Adapter{"stub": stubAdapterWithImage(img)},
	}
}

// runningAgentOnRuntime returns a steady Running stub-runtime agent (no pending
// desired-phase change).
func runningAgentOnRuntime() *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "default"},
		Spec:       kyberv1.AgentSpec{Runtime: "stub"},
		Status:     kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning},
	}
}

// --- helper unit tests (mirror TestIsSidecarSpecMismatched_*) ---

func TestExtractAgentSpecImage_ReturnsAgentContainerImage(t *testing.T) {
	pod := podWithAgentSpecImage("agent-dave", driftDesiredImage)
	if got := extractAgentSpecImage(pod); got != driftDesiredImage {
		t.Errorf("extractAgentSpecImage: got %q, want %q", got, driftDesiredImage)
	}
}

func TestExtractAgentSpecImage_NilPod_Empty(t *testing.T) {
	if got := extractAgentSpecImage(nil); got != "" {
		t.Errorf("extractAgentSpecImage(nil): got %q, want \"\"", got)
	}
}

func TestExtractAgentSpecImage_AgentContainerAbsent_Empty(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: StatusSidecarContainerName, Image: "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0"},
	}}}
	if got := extractAgentSpecImage(pod); got != "" {
		t.Errorf("extractAgentSpecImage (no agent container): got %q, want \"\"", got)
	}
}

// TestIsAgentRuntimeImageDrifted_FullRefMatch_NoDrift — recreated/steady pod
// carries the desired ref. This is the convergence half: no further roll.
func TestIsAgentRuntimeImageDrifted_FullRefMatch_NoDrift(t *testing.T) {
	pod := podWithAgentSpecImage("agent-dave", driftDesiredImage)
	if isAgentRuntimeImageDrifted(pod, driftDesiredImage) {
		t.Error("got drifted=true, want false (full refs identical — steady state)")
	}
}

// TestIsAgentRuntimeImageDrifted_DigestMismatch_Drift — the production case: same
// repo:tag, different digest. A bare-tag compare would miss it; the full-ref
// compare must fire.
func TestIsAgentRuntimeImageDrifted_DigestMismatch_Drift(t *testing.T) {
	pod := podWithAgentSpecImage("agent-dave", driftOldImage)
	if !isAgentRuntimeImageDrifted(pod, driftDesiredImage) {
		t.Error("got drifted=false, want true (same tag, different digest — the production sync case)")
	}
}

// TestIsAgentRuntimeImageDrifted_NilPod_False — reconcile can run before a pod
// exists; no drift signal then.
func TestIsAgentRuntimeImageDrifted_NilPod_False(t *testing.T) {
	if isAgentRuntimeImageDrifted(nil, driftDesiredImage) {
		t.Error("nil pod must not register drift")
	}
}

// TestIsAgentRuntimeImageDrifted_EmptyDesiredImage_False — load-bearing guard
// (kyber#360 Cause D): never roll a live agent onto an empty ref.
func TestIsAgentRuntimeImageDrifted_EmptyDesiredImage_False(t *testing.T) {
	pod := podWithAgentSpecImage("agent-dave", driftOldImage)
	if isAgentRuntimeImageDrifted(pod, "") {
		t.Error("empty desired image must not register drift")
	}
}

// TestIsAgentRuntimeImageDrifted_AgentContainerAbsent_False — pod exists but has
// no agent container in spec. No diff signal.
func TestIsAgentRuntimeImageDrifted_AgentContainerAbsent_False(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: StatusSidecarContainerName, Image: "ghcr.io/matty-v/kyber-status-sidecar:v1.3.0"},
	}}}
	if isAgentRuntimeImageDrifted(pod, driftDesiredImage) {
		t.Error("pod without agent container must not register drift")
	}
}

// --- classifyEvent trigger + guards ---

// TestClassifyEvent_RunningImageDrift_ReturnsDesiredRestarting — happy path: a
// steady Running agent whose live pod image drifts from adapter.Image() derives
// EventDesiredRestarting.
func TestClassifyEvent_RunningImageDrift_ReturnsDesiredRestarting(t *testing.T) {
	r := reconcilerWithRuntimeImage(driftDesiredImage)
	pod := podWithAgentSpecImage("agent-dave", driftOldImage)

	event, err := r.classifyEvent(context.Background(), runningAgentOnRuntime(), pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventDesiredRestarting {
		t.Errorf("Running + image drift: got %q, want %q", event, EventDesiredRestarting)
	}
}

// TestClassifyEvent_RunningImageMatch_NoRoll — no spurious roll: a Running agent
// whose live pod already runs the desired image is not rolled (the steady-state
// half of convergence).
func TestClassifyEvent_RunningImageMatch_NoRoll(t *testing.T) {
	r := reconcilerWithRuntimeImage(driftDesiredImage)
	pod := podWithAgentSpecImage("agent-dave", driftDesiredImage)

	event, err := r.classifyEvent(context.Background(), runningAgentOnRuntime(), pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event == EventDesiredRestarting {
		t.Errorf("Running + matched image: derived %q, want no roll", event)
	}
}

// TestClassifyEvent_ImageDrift_NonRunningPhases_NotRolled — phase guard: only
// steady Running agents are rolled by the drift check. Starting/Restarting/
// Creating and every dormant phase carry a drifted pod yet must NOT derive
// EventDesiredRestarting (they pick up the new image on next start via
// BuildPodSpec). Guards against churn.
func TestClassifyEvent_ImageDrift_NonRunningPhases_NotRolled(t *testing.T) {
	r := reconcilerWithRuntimeImage(driftDesiredImage)
	pod := podWithAgentSpecImage("agent-dave", driftOldImage)

	nonRunning := []kyberv1.AgentPhase{
		kyberv1.AgentPhaseStarting, kyberv1.AgentPhaseRestarting,
		kyberv1.AgentPhaseCreating, kyberv1.AgentPhaseStopped,
		kyberv1.AgentPhaseSuspended, kyberv1.AgentPhaseFailed,
		kyberv1.AgentPhaseNeedsAuth, kyberv1.AgentPhaseMemoryExhausted,
		kyberv1.AgentPhase(""), // unset
	}
	for _, ph := range nonRunning {
		agent := runningAgentOnRuntime()
		agent.Status.Phase = ph
		event, err := r.classifyEvent(context.Background(), agent, pod)
		if err != nil {
			t.Errorf("phase %q: unexpected error: %v", ph, err)
			continue
		}
		if event == EventDesiredRestarting {
			t.Errorf("phase %q + image drift: derived %q, want NOT rolled by the drift check", ph, event)
		}
	}
}

// TestClassifyEvent_RunningImageDrift_TerminatingPod_NoRoll — fail-safe: a pod
// already being deleted (DeletionTimestamp set) is not re-rolled.
func TestClassifyEvent_RunningImageDrift_TerminatingPod_NoRoll(t *testing.T) {
	r := reconcilerWithRuntimeImage(driftDesiredImage)
	pod := podWithAgentSpecImage("agent-dave", driftOldImage)
	now := metav1.Now()
	pod.DeletionTimestamp = &now

	event, err := r.classifyEvent(context.Background(), runningAgentOnRuntime(), pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event == EventDesiredRestarting {
		t.Errorf("terminating pod + image drift: derived %q, want no roll", event)
	}
}

// TestClassifyEvent_RunningImageDrift_EmptyDesiredImage_NoRoll — fail-safe: an
// empty resolved image (misconfig) must never roll a live agent.
func TestClassifyEvent_RunningImageDrift_EmptyDesiredImage_NoRoll(t *testing.T) {
	r := reconcilerWithRuntimeImage("")
	pod := podWithAgentSpecImage("agent-dave", driftOldImage)

	event, err := r.classifyEvent(context.Background(), runningAgentOnRuntime(), pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event == EventDesiredRestarting {
		t.Errorf("empty desired image + drifted pod: derived %q, want no roll", event)
	}
}

// TestClassifyEvent_RunningImageDrift_UnresolvableAdapter_NoRollNoError —
// fail-safe: a resolveAdapter error (unknown runtime) skips the drift check with
// no roll and no reconcile error — never disrupt a Running agent over an
// image-resolution hiccup.
func TestClassifyEvent_RunningImageDrift_UnresolvableAdapter_NoRollNoError(t *testing.T) {
	r := reconcilerWithRuntimeImage(driftDesiredImage) // registry has only "stub"
	agent := runningAgentOnRuntime()
	agent.Spec.Runtime = "does-not-exist"
	pod := podWithAgentSpecImage("agent-dave", driftOldImage)

	event, err := r.classifyEvent(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("classifyEvent must not propagate an adapter-resolution error: %v", err)
	}
	if event == EventDesiredRestarting {
		t.Errorf("unresolvable adapter + drifted pod: derived %q, want no roll", event)
	}
}

// TestClassifyEvent_RunningImageDrift_DesiredRestartingTakesPrecedence — an
// explicit operator desiredPhase=Restarting still wins; the drift block sits
// after it and is never reached. (Both derive EventDesiredRestarting, so this
// pins the ordering, not the event.)
func TestClassifyEvent_RunningImageDrift_DesiredRestartingStillRolls(t *testing.T) {
	r := reconcilerWithRuntimeImage(driftDesiredImage)
	agent := runningAgentOnRuntime()
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	pod := podWithAgentSpecImage("agent-dave", driftDesiredImage) // no drift

	event, err := r.classifyEvent(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventDesiredRestarting {
		t.Errorf("Running + desired=Restarting: got %q, want %q", event, EventDesiredRestarting)
	}
}

// --- envtest one-roll convergence (the core roll-loop risk) ---

// TestReconciler_RuntimeImageDrift_RollsOnceAndConverges drives the full
// reconcile loop: a Running agent whose live pod image drifts from the desired
// image is rolled exactly once (→ Restarting → pod recreated on the desired
// image → Starting), and a subsequent steady Running reconcile derives no
// further roll (the recreated pod carries the desired image). This is the
// gating AC — proof the drift roll converges and does not loop.
func TestReconciler_RuntimeImageDrift_RollsOnceAndConverges(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	scheme := buildTestScheme()
	r := newReconciler(k8sClient, scheme)

	// The stub adapter newReconciler wires resolves to this desired image.
	desiredImage := r.AdapterRegistry["stub"].Image()
	if desiredImage == "" {
		t.Fatal("test stub adapter has no image — cannot exercise drift")
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-image-drift"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("dave", "test-image-drift")
	if err := k8sClient.Create(context.Background(), agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dave", Namespace: "test-image-drift"}}
	agentKey := types.NamespacedName{Name: "dave", Namespace: "test-image-drift"}
	podKey := types.NamespacedName{Name: AgentPodName("dave"), Namespace: "test-image-drift"}

	// Bootstrap to Creating — creates the pod with the desired image.
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

	// Simulate runtime-image drift: the live pod's agent container is on an old
	// image (e.g. before an image-sync digest bump).
	livePod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, livePod); err != nil {
		t.Fatalf("getting live pod: %v", err)
	}
	if got := extractAgentSpecImage(livePod); got != desiredImage {
		t.Fatalf("precondition: bootstrap pod image = %q, want desired %q", got, desiredImage)
	}
	for i := range livePod.Spec.Containers {
		if livePod.Spec.Containers[i].Name == AgentContainerName {
			livePod.Spec.Containers[i].Image = driftOldImage
		}
	}
	if err := k8sClient.Update(context.Background(), livePod); err != nil {
		t.Fatalf("updating live pod image to simulate drift: %v", err)
	}

	// Reconcile: Running + drift → exactly one Restarting transition (deletes pod).
	reconcileN(t, r, req, 1)
	afterDrift := getAgent(t, k8sClient, agentKey)
	if afterDrift.Status.Phase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("phase after drift reconcile: got %q, want %q (the one roll)",
			afterDrift.Status.Phase, kyberv1.AgentPhaseRestarting)
	}

	// Reconcile: pod deleted → Restarting + pod gone → Starting (pod recreated on
	// the desired image).
	reconcileN(t, r, req, 1)
	afterRecreate := getAgent(t, k8sClient, agentKey)
	if afterRecreate.Status.Phase != kyberv1.AgentPhaseStarting {
		t.Fatalf("phase after pod deletion: got %q, want %q",
			afterRecreate.Status.Phase, kyberv1.AgentPhaseStarting)
	}

	// Convergence input: the recreated pod carries the desired image.
	recreated := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), podKey, recreated); err != nil {
		t.Fatalf("getting recreated pod: %v", err)
	}
	if got := extractAgentSpecImage(recreated); got != desiredImage {
		t.Fatalf("recreated pod image = %q, want desired %q (no convergence)", got, desiredImage)
	}

	// Fast-forward the recreated agent back to Running and reconcile again:
	// the drift check must derive NO further roll — steady state, converged.
	conv := getAgent(t, k8sClient, agentKey)
	convPatch := client.MergeFrom(conv.DeepCopy())
	now2 := metav1.Now()
	conv.Status.Phase = kyberv1.AgentPhaseRunning
	conv.Status.LastTransition = &now2
	if err := k8sClient.Status().Patch(context.Background(), conv, convPatch); err != nil {
		t.Fatalf("patching recreated agent to Running: %v", err)
	}

	reconcileN(t, r, req, 1)
	steady := getAgent(t, k8sClient, agentKey)
	if steady.Status.Phase == kyberv1.AgentPhaseRestarting {
		t.Errorf("steady-state reconcile on matched image re-entered Restarting (roll loop) — want no further roll")
	}
}
