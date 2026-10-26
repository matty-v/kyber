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
