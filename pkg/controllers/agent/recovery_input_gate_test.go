package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/runtimes/codex"
)

// kyber#684: NeedsAuth and MemoryExhausted mean "a human must supply something".
// They used to leave on the bare spec.desiredPhase==Running, which is
// permanently true for every agent — so the transition fired on every reconcile
// and an agent with a dead credential rebuilt its pod every ~20s forever (515
// pod creations in 53 minutes, measured in production). These tests pin the
// gate: re-entry happens once per genuinely new operator input, and never on a
// steady-state reconcile.
//
// Fake client, not envtest — the gate is a Secret read plus a string compare.

const (
	rigNS    = "kyber-system"
	rigAgent = "echo"
)

func newGateReconciler(t *testing.T, objs ...client.Object) *AgentReconciler {
	t.Helper()
	scheme := newResolverScheme(t)
	b := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&kyberv1.Agent{})
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return &AgentReconciler{
		Client:   b.Build(),
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(20),
		AdapterRegistry: map[string]pkgruntimes.Adapter{
			"codex": codex.NewAdapter(),
		},
	}
}

func needsAuthAgent(recorded string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: rigAgent, Namespace: rigNS},
		Spec: kyberv1.AgentSpec{
			Runtime:      "codex",
			DesiredPhase: kyberv1.AgentPhaseRunning,
		},
		Status: kyberv1.AgentStatus{
			Phase:         kyberv1.AgentPhaseNeedsAuth,
			RecoveryInput: recorded,
		},
	}
}

func credentialSecret(rv string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: rigAgent + "-codex-auth", Namespace: rigNS, ResourceVersion: rv,
	}}
}

// The regression itself: a NeedsAuth agent whose credential has not changed
// must NOT be handed EventDesiredRunning, no matter how many times we reconcile.
func TestRecoveryGate_NeedsAuth_HoldsWhenCredentialUnchanged(t *testing.T) {
	secret := credentialSecret("100")
	agent := needsAuthAgent("rv:" + rigAgent + "-codex-auth:100")
	r := newGateReconciler(t, agent, secret)

	for i := 0; i < 5; i++ {
		ev, err := r.classifyEvent(context.Background(), agent, nil)
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if ev != "" {
			t.Fatalf("reconcile %d raised %q — an unchanged credential must not restart the pod (kyber#684)", i, ev)
		}
	}
}

// A genuinely new credential must restart the agent — exactly once.
func TestRecoveryGate_NeedsAuth_FiresOnceOnNewCredential(t *testing.T) {
	secret := credentialSecret("200")
	agent := needsAuthAgent("rv:" + rigAgent + "-codex-auth:100") // stale
	r := newGateReconciler(t, agent, secret)

	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if ev != EventDesiredRunning {
		t.Fatalf("a re-authorized agent must restart; got %q want %q", ev, EventDesiredRunning)
	}

	// The claim must be PERSISTED, not just set on the in-memory object. Every
	// real reconcile re-reads the Agent from the API server, so if the status
	// patch silently failed the gate would reopen on the very next pass and the
	// loop would be back — while an in-memory-only assertion still went green.
	var stored kyberv1.Agent
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: rigAgent, Namespace: rigNS}, &stored); err != nil {
		t.Fatalf("re-reading agent: %v", err)
	}
	want := "rv:" + rigAgent + "-codex-auth:200"
	if stored.Status.RecoveryInput != want {
		t.Fatalf("recoveryInput not persisted: stored %q, want %q", stored.Status.RecoveryInput, want)
	}

	// A second pass, driven from the STORED object as a real reconcile would be,
	// must hold.
	ev2, err := r.classifyEvent(context.Background(), &stored, nil)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if ev2 != "" {
		t.Fatalf("the same credential fired twice (%q) — this is the infinite loop (kyber#684)", ev2)
	}
}

// An operator who stopped the agent must not get a surprise pod, even when the
// credential changes underneath them.
func TestRecoveryGate_NeedsAuth_RespectsDesiredPhase(t *testing.T) {
	secret := credentialSecret("999")
	agent := needsAuthAgent("rv:" + rigAgent + "-codex-auth:100")
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	r := newGateReconciler(t, agent, secret)

	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if ev != "" {
		t.Fatalf("desiredPhase=Stopped must suppress recovery; got %q", ev)
	}
}

// A credential Secret that does not exist yet must park the agent, not spin it,
// and must still recover once the Secret appears.
func TestRecoveryGate_NeedsAuth_MissingSecretHoldsThenRecovers(t *testing.T) {
	agent := needsAuthAgent("")
	r := newGateReconciler(t, agent)

	// First pass records the "absent" sentinel and permits one attempt.
	if _, err := r.classifyEvent(context.Background(), agent, nil); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	// Subsequent passes must hold while the Secret is still missing.
	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if ev != "" {
		t.Fatalf("a still-missing credential must not spin; got %q", ev)
	}

	// Once the operator creates it, the input differs and recovery fires.
	// No explicit resourceVersion — the API server assigns it, and the fake
	// client rejects Create requests that set one.
	fresh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: rigAgent + "-codex-auth", Namespace: rigNS,
	}}
	if err := r.Create(context.Background(), fresh); err != nil {
		t.Fatalf("creating secret: %v", err)
	}
	ev, err = r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if ev != EventDesiredRunning {
		t.Fatalf("creating the credential must recover the agent; got %q", ev)
	}
}

// MemoryExhausted carries the identical defect and gets the identical gate,
// keyed on the memory limit rather than a Secret.
func TestRecoveryGate_MemoryExhausted_HoldsUntilLimitChanges(t *testing.T) {
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: rigAgent, Namespace: rigNS},
		Spec: kyberv1.AgentSpec{
			Runtime:      "codex",
			DesiredPhase: kyberv1.AgentPhaseRunning,
			Resources:    kyberv1.AgentResources{Memory: resource.MustParse("1Gi")},
		},
		Status: kyberv1.AgentStatus{
			Phase:         kyberv1.AgentPhaseMemoryExhausted,
			RecoveryInput: "mem=1Gi",
		},
	}
	r := newGateReconciler(t, agent)

	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if ev != "" {
		t.Fatalf("an unchanged memory limit must not restart the agent; got %q", ev)
	}

	// Operator bumps the limit — that is the new input.
	agent.Spec.Resources.Memory = resource.MustParse("2Gi")
	ev, err = r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent after bump: %v", err)
	}
	if ev != EventDesiredRunning {
		t.Fatalf("bumping memory must recover the agent; got %q", ev)
	}
}

// kyber#26: the operator's Restart pod button in NeedsAuth. The PWA fires
// POST /start, and the API clears status.recoveryInput on that path before
// setting desiredPhase=Running (routes_agents.go, setAgentDesiredPhase). This
// pins the controller half of that contract: a cleared claim reopens the gate
// even though the credential Secret has NOT changed, and the very next
// reconcile re-claims it so the click buys exactly one attempt.
//
// The unchanged-credential part is the whole point. `lando` (kyber-falcon,
// 2026-08-09) sat in NeedsAuth with a valid credential whose resourceVersion
// already equalled the recorded claim; the gate held, and only a cluster-admin
// `kubectl annotate` unwedged it. Nothing in the automatic path is relaxed —
// TestRecoveryGate_NeedsAuth_HoldsWhenCredentialUnchanged still pins that the
// controller refuses to self-retry on the same input.
func TestRecoveryGate_NeedsAuth_OperatorRestartReopensGateOnce(t *testing.T) {
	const rv = "100"
	secret := credentialSecret(rv)
	// Exactly the lando shape: the claim already matches the live Secret, so
	// the automatic path is (correctly) shut.
	agent := needsAuthAgent("rv:" + rigAgent + "-codex-auth:" + rv)
	r := newGateReconciler(t, agent, secret)

	if ev, err := r.classifyEvent(context.Background(), agent, nil); err != nil || ev != "" {
		t.Fatalf("precondition: gate must be shut on an unchanged credential; got %q, err %v", ev, err)
	}

	// What POST /start does to the object. This must be PERSISTED, not just set
	// on the local copy: seeding the clear in memory only would leave the stored
	// object still holding the original claim, so the "was it re-claimed?"
	// assertion below would be comparing against the seed and would pass even if
	// recoveryInputChanged never wrote anything at all.
	clearPatch := client.MergeFrom(agent.DeepCopy())
	agent.Status.RecoveryInput = ""
	if err := r.Status().Patch(context.Background(), agent, clearPatch); err != nil {
		t.Fatalf("clearing recovery input the way POST /start does: %v", err)
	}
	var cleared kyberv1.Agent
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: rigAgent, Namespace: rigNS}, &cleared); err != nil {
		t.Fatalf("re-reading agent after clear: %v", err)
	}
	if cleared.Status.RecoveryInput != "" {
		t.Fatalf("precondition: the clear did not persist, stored %q — the re-claim assertion below would be vacuous", cleared.Status.RecoveryInput)
	}

	// Reconcile off the STORED object, as a real reconcile does.
	agent = &cleared
	ev, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent after operator restart: %v", err)
	}
	if ev != EventDesiredRunning {
		t.Fatalf("an operator restart must recover a NeedsAuth agent even on an unchanged credential; got %q want %q", ev, EventDesiredRunning)
	}

	// Re-read from the API server the way a real reconcile does: the claim must
	// have been persisted, or the gate reopens next pass and one click becomes
	// the kyber#684 loop.
	var stored kyberv1.Agent
	if err := r.Get(context.Background(),
		client.ObjectKey{Name: rigAgent, Namespace: rigNS}, &stored); err != nil {
		t.Fatalf("re-reading agent: %v", err)
	}
	if want := "rv:" + rigAgent + "-codex-auth:" + rv; stored.Status.RecoveryInput != want {
		t.Fatalf("operator restart did not re-claim the input: stored %q, want %q", stored.Status.RecoveryInput, want)
	}

	// Holding the button down (or the controller simply reconciling again) must
	// not create a second pod.
	for i := 0; i < 3; i++ {
		ev, err := r.classifyEvent(context.Background(), &stored, nil)
		if err != nil {
			t.Fatalf("follow-up reconcile %d: %v", i, err)
		}
		if ev != "" {
			t.Fatalf("follow-up reconcile %d raised %q — one click must mean one pod create (kyber#684)", i, ev)
		}
	}
}

// The transition the button ultimately rides on. Stated separately from the
// gate so a future edit that removes the row fails here with a readable
// message rather than as a mystery "invalid transition" at runtime.
func TestStateMachine_NeedsAuth_DesiredRunningRecreatesPod(t *testing.T) {
	tr, err := NextPhase(kyberv1.AgentPhaseNeedsAuth, EventDesiredRunning)
	if err != nil {
		t.Fatalf("no {NeedsAuth, EventDesiredRunning} transition — the PWA's Restart pod action in NeedsAuth (kyber#26) has nothing to fire: %v", err)
	}
	if tr.Action != ActionResetRetryAndCreatePod {
		t.Fatalf("NeedsAuth recovery must rebuild the pod; got action %q want %q", tr.Action, ActionResetRetryAndCreatePod)
	}
	if tr.NextPhase != kyberv1.AgentPhaseStarting {
		t.Fatalf("NeedsAuth recovery must land in Starting; got %q", tr.NextPhase)
	}

	// The dead-button check (#599): desiredPhase=Restarting still has no edge
	// out of NeedsAuth, which is why the PWA action is 'retry-startup' (→
	// /start) and not 'restart' (→ /restart). If someone adds this row, they
	// must also clear the sticky Restarting intent or NeedsAuth loops.
	if _, err := NextPhase(kyberv1.AgentPhaseNeedsAuth, EventDesiredRestarting); err == nil {
		t.Fatal("a {NeedsAuth, EventDesiredRestarting} row now exists — spec.desiredPhase=Restarting is sticky and is only cleared by ActionCaptureStateAndDeletePod, so this row will re-fire every reconcile unless it clears the intent too (kyber#684, kyber#26)")
	}
}
