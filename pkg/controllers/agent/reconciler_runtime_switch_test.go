package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestClearStaleRuntimeStatusAfterSwitch(t *testing.T) {
	consistent := &kyberv1.Agent{Spec: kyberv1.AgentSpec{Runtime: "codex"}, Status: kyberv1.AgentStatus{Runtime: kyberv1.AgentRuntimeStatus{Runtime: "codex", InstalledVersion: "1.0"}}}
	if clearStaleRuntimeStatus(consistent) || consistent.Status.Runtime.InstalledVersion != "1.0" {
		t.Fatal("matching runtime observation was cleared")
	}
	stale := &kyberv1.Agent{Spec: kyberv1.AgentSpec{Runtime: "claude-code"}, Status: kyberv1.AgentStatus{
		CurrentModel: "gpt-old", Runtime: kyberv1.AgentRuntimeStatus{Runtime: "codex", InstalledVersion: "1.0", Capabilities: &kyberv1.RuntimeCapabilitiesObservation{PodUID: "old-pod"}},
		Conditions: []metav1.Condition{{Type: kyberv1.AgentConditionRuntimeUnusable, Status: metav1.ConditionTrue, Reason: "ProbeFailed", Message: "old harness"}},
	}}
	if !clearStaleRuntimeStatus(stale) || stale.Status.Runtime.Runtime != "" || stale.Status.Runtime.Capabilities != nil || stale.Status.CurrentModel != "" || len(stale.Status.Conditions) != 0 {
		t.Fatalf("stale runtime observation retained: %+v", stale.Status)
	}
}

func TestRuntimeSwitchNeedsAuthKeepsPVCAndSourceCredential(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()
	ctx := context.Background()
	namespace := "test-runtime-switch"
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	r := newReconciler(k8sClient, buildTestScheme())
	r.AdapterRegistry["codex"] = r.AdapterRegistry["claude-code"]
	agent := newTestAgent("switcher", namespace)
	agent.Spec.Runtime = "codex"
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Name: agent.Name, Namespace: namespace}
	req := ctrl.Request{NamespacedName: key}
	reconcileN(t, r, req, 1)
	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := types.NamespacedName{Name: PVCName(agent.Name), Namespace: namespace}
	if err := k8sClient.Get(ctx, pvcKey, pvc); err != nil {
		t.Fatal(err)
	}
	pvcUID := pvc.UID
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent.Name + "-codex-auth", Namespace: namespace}, Data: map[string][]byte{"auth.json": []byte(`{"saved":true}`)}}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	secretUID := secret.UID
	current := getAgent(t, k8sClient, key)
	statusBefore := current.DeepCopy()
	current.Status.Phase = kyberv1.AgentPhaseRunning
	current.Status.Runtime = kyberv1.AgentRuntimeStatus{Runtime: "codex", InstalledVersion: "old"}
	current.Status.CurrentModel = "gpt-old"
	if err := k8sClient.Status().Patch(ctx, current, client.MergeFrom(statusBefore)); err != nil {
		t.Fatal(err)
	}
	current = getAgent(t, k8sClient, key)
	before := current.DeepCopy()
	current.Spec.Runtime = "claude-code"
	current.Spec.Model = ""
	current.Spec.RuntimeVersion = ""
	current.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	if err := k8sClient.Patch(ctx, current, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, req, 1)
	got := getAgent(t, k8sClient, key)
	if got.Status.Phase != kyberv1.AgentPhaseNeedsAuth || got.Status.Runtime.Runtime != "" || got.Status.CurrentModel != "" {
		t.Fatalf("switched status=%+v", got.Status)
	}
	if err := k8sClient.Get(ctx, pvcKey, pvc); err != nil || pvc.UID != pvcUID {
		t.Fatalf("PVC changed: uid=%q err=%v", pvc.UID, err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil || secret.UID != secretUID {
		t.Fatalf("source credential changed: uid=%q err=%v", secret.UID, err)
	}
}
