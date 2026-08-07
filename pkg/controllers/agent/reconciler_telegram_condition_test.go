package agent

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
)

// kyber#684 — an install that enables Telegram but never pinned
// image.telegramSidecar.tag.
//
// Before the convergence this was survivable for Claude Code: the native plugin
// carried the channel, so a missing sidecar image cost nothing. Now the plugin
// is gone and the runtime holds no bot token, so an unpinned image means the
// agent has NO Telegram — and to an operator that reads as "the agent stopped
// answering me", with nothing anywhere naming the install as the cause.
//
// A fresh install is exactly that today: established clusters all pin
// the sidecar image and the new install does not.
//
// These tests assert the condition reaches the API SERVER. The first cut set it
// on the in-memory object inside createPod and never patched, so every
// assertion against `agent.Status.Conditions` passed while the PWA and kubectl
// saw nothing at all.

// wiredTelegram is the "Secret and allowlist are both fine" state, so these
// tests exercise the sidecar-image reason in isolation.
var wiredTelegram = TelegramWiring{SecretExists: true, HasAllowlist: true}

func telegramCondAgent(name string, enabled bool) *kyberv1.Agent {
	ag := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       kyberv1.AgentSpec{Runtime: "stub", Model: "claude-opus-5"},
	}
	ag.Spec.Secrets.TelegramEnabled = enabled
	return ag
}

func reconcilerWithSidecarImage(img string, objs ...*kyberv1.Agent) *AgentReconciler {
	b := fake.NewClientBuilder().WithScheme(buildTestScheme())
	for _, o := range objs {
		b = b.WithStatusSubresource(o).WithObjects(o)
	}
	return &AgentReconciler{
		Client:               b.Build(),
		Scheme:               buildTestScheme(),
		Recorder:             record.NewFakeRecorder(16),
		AdapterRegistry:      map[string]pkgruntimes.Adapter{"stub": stubAdapterWithImage("kyber/stub:v1")},
		TelegramSidecarImage: img,
	}
}

// storedCondition reads the condition back from the API server rather than the
// in-memory object. This is the assertion that matters: an unpatched condition
// is invisible to every operator surface, which defeats the entire point of
// having one.
func storedCondition(t *testing.T, r *AgentReconciler, ag *kyberv1.Agent) *metav1.Condition {
	t.Helper()
	var fetched kyberv1.Agent
	key := client.ObjectKey{Namespace: ag.Namespace, Name: ag.Name}
	if err := r.Get(context.Background(), key, &fetched); err != nil {
		t.Fatalf("fetching agent back: %v", err)
	}
	return meta.FindStatusCondition(fetched.Status.Conditions, kyberv1.AgentConditionTelegramUnavailable)
}

func TestReconcileTelegramCondition_PersistsWhenSidecarImageMissing(t *testing.T) {
	ag := telegramCondAgent("hk-47", true)
	r := reconcilerWithSidecarImage("", ag)

	r.reconcileTelegramCondition(context.Background(), ag, wiredTelegram)

	cond := storedCondition(t, r, ag)
	if cond == nil {
		t.Fatal("TelegramUnavailable was not persisted — the operator has no signal that the channel is gone")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition status=%v, want True", cond.Status)
	}
	if !strings.Contains(cond.Message, "image.telegramSidecar.tag") {
		t.Errorf("message must name the Helm value to set (the PWA renders it verbatim), got: %q", cond.Message)
	}
}

// Recovery. The condition's own message promises "this clears automatically"
// once the image is pinned, and that has to be true — a permanent false alarm
// on a healthy agent trains operators to ignore the surface.
//
// Note this cannot be tested through createPod: a running agent never re-enters
// pod construction, which is precisely why this condition lives on the
// reconcile path and the runtime-image one does not.
func TestReconcileTelegramCondition_ClearsOnceImageIsPinned(t *testing.T) {
	ag := telegramCondAgent("hk-47", true)
	broken := reconcilerWithSidecarImage("", ag)
	broken.reconcileTelegramCondition(context.Background(), ag, wiredTelegram)
	if storedCondition(t, broken, ag) == nil {
		t.Fatal("setup: expected the condition to be set")
	}

	fixed := reconcilerWithSidecarImage("ghcr.io/matty-v/kyber-mcp-telegram:v2.6.3", ag)
	fixed.reconcileTelegramCondition(context.Background(), ag, wiredTelegram)
	if cond := storedCondition(t, fixed, ag); cond != nil {
		t.Errorf("condition did not clear after the image was pinned: %+v", cond)
	}
}

// An agent that never asked for Telegram must not carry a Telegram condition,
// whatever the install pins.
func TestReconcileTelegramCondition_SilentWhenTelegramDisabled(t *testing.T) {
	ag := telegramCondAgent("dave", false)
	r := reconcilerWithSidecarImage("", ag)

	r.reconcileTelegramCondition(context.Background(), ag, wiredTelegram)

	if cond := storedCondition(t, r, ag); cond != nil {
		t.Errorf("agent with telegramEnabled=false got a Telegram condition: %+v", cond)
	}
}

// Unlike RuntimeImageMissing, this fault must NOT block the pod. Telegram is one
// channel, not the agent's reason to exist — an agent with a dead channel should
// still run, do its work, and be reachable every other way.
func TestReconcileTelegramCondition_DoesNotBlockPodCreation(t *testing.T) {
	ag := telegramCondAgent("hk-47", true)
	r := reconcilerWithSidecarImage("", ag)

	if err := r.createPod(context.Background(), ag); err != nil {
		t.Fatalf("a missing Telegram sidecar image must not stop the agent from running, got: %v", err)
	}
}
