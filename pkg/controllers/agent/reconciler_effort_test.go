package agent

import (
	"context"
	"testing"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEffortDirectSpecFailureIsVisibleAndClearable(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	a := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "effort-test", Namespace: "test"}, Spec: kyberv1.AgentSpec{Runtime: "codex", RuntimeVersion: "future", Model: "gpt-5.6-sol", Effort: "high"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(a).WithObjects(a).Build()
	r := &AgentReconciler{Client: c}
	key := types.NamespacedName{Namespace: a.Namespace, Name: a.Name}
	if err := c.Get(ctx, key, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolveAgentForPod(ctx, a); err == nil {
		t.Fatal("unsupported direct CRD effort accepted")
	}
	if err := c.Get(ctx, key, a); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(a.Status.Conditions, kyberv1.AgentConditionEffortUnsupported) {
		t.Fatal("unsupported effort has no operator-visible condition")
	}
	a.Spec.Effort = ""
	if err := c.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolveAgentForPod(ctx, a); err != nil {
		t.Fatalf("Default did not recover unknown version: %v", err)
	}
	if err := c.Get(ctx, key, a); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(a.Status.Conditions, kyberv1.AgentConditionEffortUnsupported) != nil {
		t.Fatal("reset left stale effort error")
	}
}

func TestEffortEditDoesNotRestartExhaustedFailedAgent(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	a := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "failed-effort", Namespace: "test", Generation: 2, Annotations: map[string]string{kyberv1.AnnotationPassiveConfigGeneration: "2"}}, Spec: kyberv1.AgentSpec{DesiredPhase: kyberv1.AgentPhaseRunning, Effort: "high"}, Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseFailed, ObservedGeneration: 1, RestartCount: maxRestartRetries}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(a).WithObjects(a).Build()
	r := &AgentReconciler{Client: c}
	event, err := r.classifyEvent(ctx, a, nil)
	if err != nil || event != EventRetryLimitReached {
		t.Fatalf("passive edit event=%s err=%v", event, err)
	}
	if a.Status.ObservedGeneration != 1 || a.Status.RestartCount != maxRestartRetries {
		t.Fatal("passive edit changed retry state")
	}
	// A subsequent independent repair advances generation and retains the existing
	// recovery path instead of being suppressed forever by the effort marker.
	a.Generation = 3
	if err := c.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	event, err = r.classifyEvent(ctx, a, nil)
	if err != nil || event != EventDesiredRunning {
		t.Fatalf("repair event=%s err=%v", event, err)
	}
}
