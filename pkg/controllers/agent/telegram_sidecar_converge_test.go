package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// kyber#688 — the Telegram bridge has to converge onto RUNNING pods, not only
// pods that happen to get rebuilt for some unrelated reason.
//
// Two live production failures motivate every case below: `dave`
// stayed on the retired in-process plugin for 68 minutes after the control
// plane had migrated him (presence drift), and `r2-d2` kept serving from a
// superseded bridge digest until a hand-rolled pod delete (image drift).

const (
	tgCurrent = "ghcr.io/matty-v/kyber-mcp-telegram@sha256:fef8b2a9"
	tgStale   = "ghcr.io/matty-v/kyber-mcp-telegram@sha256:283f17a6"
)

// telegramPod builds an agent pod. bridgeImage=="" means the pod has NO
// kyber-mcp-telegram container — the un-migrated shape.
func telegramPod(name, bridgeImage string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kyber",
			Labels:    map[string]string{"kyber.io/agent": "dave"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: AgentContainerName, Image: "ghcr.io/matty-v/kyber-claude-code:v1"},
			},
			InitContainers: []corev1.Container{
				{Name: StatusSidecarContainerName, Image: "ghcr.io/matty-v/kyber-status-sidecar:v1"},
			},
		},
	}
	if bridgeImage != "" {
		p.Spec.Containers = append(p.Spec.Containers,
			corev1.Container{Name: TelegramSidecarContainerName, Image: bridgeImage})
	}
	return p
}

// telegramIdleAgent is an idle, Telegram-enabled agent — the shape that is
// eligible to converge once everything else lines up.
func telegramIdleAgent() *kyberv1.Agent {
	a := idleAgent("dave", "kyber")
	a.Spec.Secrets.TelegramEnabled = true
	return a
}

// telegramWired is the TelegramWiring of an agent whose allowlist is in place.
var telegramWired = TelegramWiring{SecretExists: true, HasAllowlist: true}

// newTelegramConvergeReconciler builds a reconciler pinned to controllerImage
// with the given seed objects.
func newTelegramConvergeReconciler(t *testing.T, controllerImage string, objs ...client.Object) *AgentReconciler {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(schedulingTestScheme(t)).
		WithObjects(objs...).
		Build()
	return &AgentReconciler{
		Client:               c,
		Recorder:             record.NewFakeRecorder(16),
		TelegramSidecarImage: controllerImage,
	}
}

func podStillThere(t *testing.T, r *AgentReconciler, pod *corev1.Pod) {
	t.Helper()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Errorf("pod must not be deleted; got err=%v", err)
	}
}

// --- drift predicate ---------------------------------------------------

// TestIsTelegramSidecarDrifted_MissingContainer_IsDrift is the `dave` case and
// the single behavioral difference from the status-sidecar predicate: for an
// opt-in channel, "no bridge container" is drift, not "no signal".
func TestIsTelegramSidecarDrifted_MissingContainer_IsDrift(t *testing.T) {
	if !isTelegramSidecarDrifted(telegramPod("agent-dave", ""), tgCurrent) {
		t.Fatal("a pod with no kyber-mcp-telegram container must count as drifted")
	}
}

// TestIsTelegramSidecarDrifted_StaleImage_IsDrift is the `r2-d2` case.
func TestIsTelegramSidecarDrifted_StaleImage_IsDrift(t *testing.T) {
	if !isTelegramSidecarDrifted(telegramPod("agent-dave", tgStale), tgCurrent) {
		t.Fatal("a pod on a superseded bridge digest must count as drifted")
	}
}

func TestIsTelegramSidecarDrifted_CurrentImage_NoDrift(t *testing.T) {
	if isTelegramSidecarDrifted(telegramPod("agent-dave", tgCurrent), tgCurrent) {
		t.Fatal("a pod already on the current bridge must not count as drifted")
	}
}

// TestIsTelegramSidecarDrifted_EmptyDesiredImage_NoDrift mirrors the
// load-bearing empty-image guard on the runtime and status-sidecar paths.
// Without it an install that never pinned image.telegramSidecar would see every
// Telegram agent read as permanently drifted.
func TestIsTelegramSidecarDrifted_EmptyDesiredImage_NoDrift(t *testing.T) {
	if isTelegramSidecarDrifted(telegramPod("agent-dave", ""), "") {
		t.Fatal("an unset TelegramSidecarImage must never register drift")
	}
}

func TestIsTelegramSidecarDrifted_NilPod_NoDrift(t *testing.T) {
	if isTelegramSidecarDrifted(nil, tgCurrent) {
		t.Fatal("nil pod must not register drift")
	}
}

// TestIsSidecarSpecMismatched_AbsenceStillNotDrift pins the half of the
// generalization that must NOT change: the status sidecar is unconditional, so
// an absent one is mid-rebuild, not a reason to delete a pod.
func TestIsSidecarSpecMismatched_AbsenceStillNotDrift(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: AgentContainerName, Image: "x:1"}},
	}}
	if isSidecarSpecMismatched(pod, "ghcr.io/matty-v/kyber-status-sidecar:v2") {
		t.Fatal("absence must remain non-drift for the status sidecar (kyber#358 behavior)")
	}
}

// --- convergence ------------------------------------------------------

// TestConvergeTelegramSidecar_MissingBridge_RollsPod is the headline fix: an
// idle, migrated agent whose pod predates the sidecar gets rolled onto it
// without an operator touching anything.
func TestConvergeTelegramSidecar_MissingBridge_RollsPod(t *testing.T) {
	pod := telegramPod("agent-dave", "")
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)
	r.telegramCanary.markVerified(tgCurrent) // steady state; canary already passed

	rolled, err := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("expected the un-migrated pod to be rolled (the 68-minute `dave` bug)")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err == nil {
		t.Error("pod should have been deleted so the next reconcile rebuilds it with the bridge")
	}
}

// TestConvergeTelegramSidecar_StaleImage_RollsPod — bumping
// image.telegramSidecar.tag reaches running pods with no manual delete.
func TestConvergeTelegramSidecar_StaleImage_RollsPod(t *testing.T) {
	pod := telegramPod("agent-dave", tgStale)
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)
	r.telegramCanary.markVerified(tgCurrent)

	rolled, err := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("expected a digest bump to roll the pod (the `r2-d2` bug)")
	}
}

func TestConvergeTelegramSidecar_AlreadyCurrent_NoOp(t *testing.T) {
	pod := telegramPod("agent-dave", tgCurrent)
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)
	r.telegramCanary.markVerified(tgCurrent)

	rolled, err := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("a pod already on the current bridge must not be rolled")
	}
	podStillThere(t, r, pod)
}

// TestConvergeTelegramSidecar_TelegramDisabled_NeverRolls — an agent that never
// asked for Telegram has no bridge by design, and must not be read as drifted.
func TestConvergeTelegramSidecar_TelegramDisabled_NeverRolls(t *testing.T) {
	pod := telegramPod("agent-dave", "")
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)
	r.telegramCanary.markVerified(tgCurrent)
	agent := idleAgent("dave", "kyber") // TelegramEnabled stays false

	rolled, err := r.convergeTelegramSidecar(context.Background(), agent, pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("an agent without Telegram enabled must never be rolled for the bridge")
	}
	podStillThere(t, r, pod)
}

// TestConvergeTelegramSidecar_NoTelegramSidecarImage_NeverRolls is the guard
// that keeps an unpinned install from rolling its Telegram agents forever:
// every rebuild would produce the same bridge-less pod, so the drift would
// never clear. reconcileTelegramCondition already surfaces this install fault.
func TestConvergeTelegramSidecar_NoTelegramSidecarImage_NeverRolls(t *testing.T) {
	pod := telegramPod("agent-dave", "")
	r := newTelegramConvergeReconciler(t, "", pod)

	rolled, err := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("an unset TelegramSidecarImage must never delete a pod")
	}
	podStillThere(t, r, pod)
}

// --- gates -------------------------------------------------------------

// TestConvergeTelegramSidecar_IdleGate_DefersOnWorking — a stale bridge is
// never worth interrupting a working agent for.
func TestConvergeTelegramSidecar_IdleGate_DefersOnWorking(t *testing.T) {
	pod := telegramPod("agent-dave", "")
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)
	r.telegramCanary.markVerified(tgCurrent)
	agent := workingAgent("dave", "kyber")
	agent.Spec.Secrets.TelegramEnabled = true

	rolled, err := r.convergeTelegramSidecar(context.Background(), agent, pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("idle gate must block the roll on a Working agent")
	}
	podStillThere(t, r, pod)
}

// TestConvergeTelegramSidecar_NilActivity_Defers — unknown state is treated as
// working, same conservative posture as 5c/5d.
func TestConvergeTelegramSidecar_NilActivity_Defers(t *testing.T) {
	pod := telegramPod("agent-dave", "")
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)
	r.telegramCanary.markVerified(tgCurrent)
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "kyber"}}
	agent.Spec.Secrets.TelegramEnabled = true

	rolled, err := r.convergeTelegramSidecar(context.Background(), agent, pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("nil Activity must block the roll")
	}
}

// TestConvergeTelegramSidecar_NoAllowlist_Defers — the bridge exits without an
// allowlist, so rolling here would trade a working agent for a crash-looping
// container and still no Telegram.
func TestConvergeTelegramSidecar_NoAllowlist_Defers(t *testing.T) {
	pod := telegramPod("agent-dave", "")
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)
	r.telegramCanary.markVerified(tgCurrent)

	rolled, err := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), pod,
		TelegramWiring{SecretExists: true, HasAllowlist: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("must not roll an agent whose bridge has no allowlist to start with")
	}
	podStillThere(t, r, pod)
}

// TestConvergeTelegramSidecar_ConcurrencyCap_Defers — shares one cluster-wide
// delete budget with 5c, 5d and the runtime roll.
func TestConvergeTelegramSidecar_ConcurrencyCap_Defers(t *testing.T) {
	now := metav1.Now()
	deleting := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "agent-r2-d2", Namespace: "kyber",
		Labels:            map[string]string{"kyber.io/agent": "r2-d2"},
		DeletionTimestamp: &now,
		Finalizers:        []string{"kyber.io/agent-cleanup"},
	}}
	pod := telegramPod("agent-dave", "")
	r := newTelegramConvergeReconciler(t, tgCurrent, pod, deleting)
	r.telegramCanary.markVerified(tgCurrent)

	rolled, err := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("must defer while another agent pod is mid-deletion")
	}
	podStillThere(t, r, pod)
}

// TestConvergeTelegramSidecar_BadPin_ContainedToCanary is the failure mode that
// matters most: a bad telegramSidecar pin must cost one agent, not the whole
// Telegram fleet. Agent one is the canary and rolls; agent two holds while the
// window is open; once the window elapses the image is marked failed and every
// further roll is held behind a TelegramSidecarRollHeld event.
func TestConvergeTelegramSidecar_BadPin_ContainedToCanary(t *testing.T) {
	const bad = "ghcr.io/matty-v/kyber-mcp-telegram:does-not-exist"
	davePod := telegramPod("agent-dave", tgStale)
	r2Pod := telegramPod("agent-r2-d2", tgStale)
	r2Pod.Labels["kyber.io/agent"] = "r2-d2"

	r := newTelegramConvergeReconciler(t, bad, davePod, r2Pod)
	r.TelegramSidecarCanaryWindow = 50 * time.Millisecond

	// Agent one: no canary yet for this image, so it becomes the canary.
	rolled, err := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), davePod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rolled {
		t.Fatal("the first eligible agent must roll as the canary")
	}

	// Agent two, canary still mid-window: hold, silently.
	r2 := telegramIdleAgent()
	r2.Name = "r2-d2"
	rolled, err = r.convergeTelegramSidecar(context.Background(), r2, r2Pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("a second agent must not roll while the canary window is open")
	}

	// Window elapses with nothing ever observed Ready on the bad image.
	time.Sleep(60 * time.Millisecond)
	rolled, err = r.convergeTelegramSidecar(context.Background(), r2, r2Pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("rolls must stay held once the canary window elapsed without a Ready bridge")
	}
	if !r.telegramCanary.failedCanary(bad) {
		t.Fatal("the bad image should be marked canary-failed")
	}
	podStillThere(t, r, r2Pod)

	// And the hold is visible to an operator rather than silent.
	rec := r.Recorder.(*record.FakeRecorder)
	var held bool
	for len(rec.Events) > 0 {
		if ev := <-rec.Events; strings.Contains(ev, "TelegramSidecarRollHeld") {
			held = true
		}
	}
	if !held {
		t.Error("expected a TelegramSidecarRollHeld event once the canary failed")
	}
}

// TestConvergeTelegramSidecar_VerifiedImage_ConvergesFleet — the other side of
// the canary: once any pod is seen Ready on the image, the rest converge at
// steady state rather than one-at-a-canary.
func TestConvergeTelegramSidecar_VerifiedImage_ConvergesFleet(t *testing.T) {
	pod := telegramPod("agent-dave", tgStale)
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)

	// Nothing verified yet and no canary: this pod becomes the canary and rolls.
	rolled, _ := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), pod, telegramWired)
	if !rolled {
		t.Fatal("first eligible pod should roll as the canary")
	}

	// Its replacement comes back Ready on the new image — the 5a.2 trigger.
	replacement := telegramPod("agent-dave", tgCurrent)
	replacement.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  TelegramSidecarContainerName,
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if !isTelegramSidecarReady(replacement) {
		t.Fatal("a Ready+Running bridge container must read as ready")
	}
	r.telegramCanary.markVerified(tgCurrent)

	if !r.telegramCanary.wasVerified(tgCurrent) {
		t.Fatal("image should be verified after a Ready observation")
	}
}

// TestConvergeTelegramSidecar_TerminatingPod_NoOp — never stack a delete on a
// pod already going away.
func TestConvergeTelegramSidecar_TerminatingPod_NoOp(t *testing.T) {
	now := metav1.Now()
	pod := telegramPod("agent-dave", "")
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"kyber.io/agent-cleanup"}
	r := newTelegramConvergeReconciler(t, tgCurrent, pod)
	r.telegramCanary.markVerified(tgCurrent)

	rolled, err := r.convergeTelegramSidecar(context.Background(), telegramIdleAgent(), pod, telegramWired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rolled {
		t.Fatal("a terminating pod must not be re-deleted")
	}
}
