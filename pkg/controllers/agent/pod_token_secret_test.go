package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/podtoken"
)

func TestEnsurePodTokenSecret_MintsLabeledOwnedSecret(t *testing.T) {
	scheme := buildTestScheme()
	agent := newTestAgent("dave", "kyber-system")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme, PodTokenKey: []byte("kyber-566-reconciler-test-key-aaaa")}
	ctx := context.Background()

	if err := r.ensurePodTokenSecret(ctx, agent); err != nil {
		t.Fatalf("ensurePodTokenSecret: %v", err)
	}
	// Idempotent re-run must not error or duplicate.
	if err := r.ensurePodTokenSecret(ctx, agent); err != nil {
		t.Fatalf("second ensurePodTokenSecret (idempotent): %v", err)
	}

	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: PodTokenSecretName("dave"), Namespace: "kyber-system"}
	if err := c.Get(ctx, key, sec); err != nil {
		t.Fatalf("pod-token secret not created: %v", err)
	}

	// Labeled kyber.io/agent=<name> so the deletion finalizer GCs it.
	if sec.Labels["kyber.io/agent"] != "dave" {
		t.Errorf("label kyber.io/agent: got %q, want dave", sec.Labels["kyber.io/agent"])
	}
	// Owner-ref'd to the Agent.
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != "dave" {
		t.Errorf("owner reference: got %+v, want one ref to dave", sec.OwnerReferences)
	}
	// The stored token verifies to the agent's own identity.
	tok := string(sec.Data[PodTokenSecretKey])
	got, err := podtoken.Parse(tok, r.PodTokenKey)
	if err != nil {
		t.Fatalf("stored token does not verify: %v", err)
	}
	if got != "dave" {
		t.Errorf("token identity: got %q, want dave", got)
	}
}

func TestEnsurePodTokenSecret_NoKey_Skips(t *testing.T) {
	scheme := buildTestScheme()
	agent := newTestAgent("dave", "kyber-system")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	r := &AgentReconciler{Client: c, Scheme: scheme} // no PodTokenKey
	ctx := context.Background()

	if err := r.ensurePodTokenSecret(ctx, agent); err != nil {
		t.Fatalf("ensurePodTokenSecret with no key should no-op, got: %v", err)
	}
	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: PodTokenSecretName("dave"), Namespace: "kyber-system"}
	if err := c.Get(ctx, key, sec); err == nil {
		t.Fatal("no signing key configured but a pod-token secret was created")
	}
}

func TestEnsurePodTokenSecret_RotatesOnKeyChange(t *testing.T) {
	scheme := buildTestScheme()
	agent := newTestAgent("dave", "kyber-system")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	ctx := context.Background()

	r1 := &AgentReconciler{Client: c, Scheme: scheme, PodTokenKey: []byte("old-key-xxxxxxxxxxxxxxxxxxxxxxxxxx")}
	if err := r1.ensurePodTokenSecret(ctx, agent); err != nil {
		t.Fatalf("mint with old key: %v", err)
	}

	r2 := &AgentReconciler{Client: c, Scheme: scheme, PodTokenKey: []byte("new-key-yyyyyyyyyyyyyyyyyyyyyyyyyy")}
	if err := r2.ensurePodTokenSecret(ctx, agent); err != nil {
		t.Fatalf("re-mint after key rotation: %v", err)
	}

	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: PodTokenSecretName("dave"), Namespace: "kyber-system"}
	if err := c.Get(ctx, key, sec); err != nil {
		t.Fatalf("get rotated secret: %v", err)
	}
	// The stored token must now verify under the NEW key, not the old one.
	if _, err := podtoken.Parse(string(sec.Data[PodTokenSecretKey]), r2.PodTokenKey); err != nil {
		t.Errorf("token not re-signed under the new key: %v", err)
	}
	if _, err := podtoken.Parse(string(sec.Data[PodTokenSecretKey]), r1.PodTokenKey); err == nil {
		t.Error("token still verifies under the old key after rotation")
	}
}
