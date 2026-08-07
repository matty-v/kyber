package agent

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
)

// kyber#674 — a runtime registered with no image configured.
//
// Before this, the empty image flowed into containers[0].image, the API server
// rejected the pod, and Reconcile returned before updatePhase ever ran. The
// agent was left with a COMPLETELY BLANK status — no phase, no message, no
// condition — while the controller retried every ~20s forever. The only trace
// was a control-plane log line. Observed in production bringing up HK-47,
// the first prod Codex agent.
//
// These tests pin the two things that make it visible and non-looping: the
// condition is set with a remediation message, and no invalid pod is attempted.

func agentOnStubRuntime(name string) *kyberv1.Agent {
	return &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       kyberv1.AgentSpec{Runtime: "stub"},
	}
}

// reconcilerWithStubImage is like reconcilerWithRuntimeImage but seeds the
// agent into the fake client so status patches have something to write to.
func reconcilerWithStubImage(img string, objs ...*kyberv1.Agent) *AgentReconciler {
	b := fake.NewClientBuilder().WithScheme(buildTestScheme())
	for _, o := range objs {
		b = b.WithStatusSubresource(o).WithObjects(o)
	}
	return &AgentReconciler{
		Client:          b.Build(),
		Scheme:          buildTestScheme(),
		Recorder:        record.NewFakeRecorder(16),
		AdapterRegistry: map[string]pkgruntimes.Adapter{"stub": stubAdapterWithImage(img)},
	}
}

func TestReconcileRuntimeImageCondition_SetsConditionWhenImageEmpty(t *testing.T) {
	ag := agentOnStubRuntime("hk-47")
	r := reconcilerWithStubImage("", ag)

	err := r.reconcileRuntimeImageCondition(context.Background(), ag, r.AdapterRegistry["stub"])
	if err == nil {
		t.Fatal("want an error so createPod aborts before building an invalid pod; got nil")
	}
	// The error must point at the fix, not just state the symptom.
	if !strings.Contains(err.Error(), "image.stub.tag") {
		t.Errorf("error should name the Helm value to set, got: %v", err)
	}

	cond := meta.FindStatusCondition(ag.Status.Conditions, kyberv1.AgentConditionRuntimeImageMissing)
	if cond == nil {
		t.Fatal("RuntimeImageMissing condition not set — this is the ONLY operator-visible signal")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition status=%v, want True", cond.Status)
	}
	if !strings.Contains(cond.Message, "image.stub.tag") {
		t.Errorf("condition message must carry the remediation verbatim (the PWA renders it), got: %q", cond.Message)
	}
}

func TestReconcileRuntimeImageCondition_NoConditionWhenImageConfigured(t *testing.T) {
	ag := agentOnStubRuntime("dave")
	r := reconcilerWithStubImage("ghcr.io/matty-v/kyber-claude-code:v2.6.2", ag)

	if err := r.reconcileRuntimeImageCondition(context.Background(), ag, r.AdapterRegistry["stub"]); err != nil {
		t.Fatalf("configured image must not error: %v", err)
	}
	if meta.FindStatusCondition(ag.Status.Conditions, kyberv1.AgentConditionRuntimeImageMissing) != nil {
		t.Error("condition set despite a configured image")
	}
}

// Recovery: once an operator pins the image, the condition must clear on its
// own. Otherwise the PWA would show a permanent false alarm on a healthy agent.
func TestReconcileRuntimeImageCondition_ClearsAfterImageConfigured(t *testing.T) {
	ag := agentOnStubRuntime("hk-47")
	broken := reconcilerWithStubImage("", ag)
	if err := broken.reconcileRuntimeImageCondition(context.Background(), ag, broken.AdapterRegistry["stub"]); err == nil {
		t.Fatal("setup: expected the missing-image error")
	}
	if meta.FindStatusCondition(ag.Status.Conditions, kyberv1.AgentConditionRuntimeImageMissing) == nil {
		t.Fatal("setup: expected the condition to be set")
	}

	fixed := reconcilerWithStubImage("ghcr.io/matty-v/kyber-codex:v2.6.2", ag)
	if err := fixed.reconcileRuntimeImageCondition(context.Background(), ag, fixed.AdapterRegistry["stub"]); err != nil {
		t.Fatalf("after pinning the image: %v", err)
	}
	if meta.FindStatusCondition(ag.Status.Conditions, kyberv1.AgentConditionRuntimeImageMissing) != nil {
		t.Error("condition did not clear after the image was configured")
	}
}

// createPod must abort BEFORE attempting the create. The old behaviour left a
// rejected-create error looping every ~20s; the point of the guard is that we
// never ask the API server for an invalid pod at all.
func TestCreatePod_AbortsWithoutCreatingWhenImageMissing(t *testing.T) {
	ag := agentOnStubRuntime("hk-47")
	r := reconcilerWithStubImage("", ag)

	err := r.createPod(context.Background(), ag)
	if err == nil {
		t.Fatal("createPod must fail when the runtime has no image")
	}
	if strings.Contains(err.Error(), "Required value") {
		t.Errorf("reached the API server with an invalid pod instead of failing early: %v", err)
	}
	if !strings.Contains(err.Error(), "no image configured") {
		t.Errorf("error should explain the configuration fault, got: %v", err)
	}
}

func TestHelmImageKey(t *testing.T) {
	for _, tc := range []struct{ runtime, want string }{
		{"codex", "codex"},
		{"claude-code", "claudeCode"},
		{"openclaw", "openclaw"},
	} {
		if got := pkgruntimes.HelmImageKey(tc.runtime); got != tc.want {
			t.Errorf("HelmImageKey(%q)=%q, want %q", tc.runtime, got, tc.want)
		}
	}
}
