package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/runtimes/codex"
)

// The real API server must persist the signal and recovery input while the
// controller removes the pod. A new pod UID must not inherit the old signal.
func TestInferenceAuthRejected_Envtest(t *testing.T) {
	k8s, teardown := setupEnvtest(t)
	defer teardown()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-inference-auth"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "auth-inference", Namespace: ns.Name}}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	agent := newTestAgent("auth", ns.Name)
	agent.Spec.Runtime = "codex"
	agent.Spec.Inference = &kyberv1.AgentInference{
		BaseURL: "https://example.com/v1", API: "openai",
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: secret.Name, Key: "token"},
	}
	if err := k8s.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: AgentPodName(agent.Name), Namespace: ns.Name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName, Image: "runtime:test"}}},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	agent = getAgent(t, k8s, client.ObjectKeyFromObject(agent))
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	agent.Status.Activity = &kyberv1.ActivityStatus{InferenceAuthRejectedPodUID: "old-pod"}
	if err := k8s.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent = getAgent(t, k8s, client.ObjectKeyFromObject(agent))
	r := newReconciler(k8s, k8s.Scheme())
	r.AdapterRegistry = map[string]pkgruntimes.Adapter{"codex": codex.NewAdapter()}
	if stale, err := r.classifyEvent(ctx, agent, pod); err != nil || stale != "" {
		t.Fatalf("stale pod signal raised event=%q err=%v", stale, err)
	}
	agent.Status.Activity.InferenceAuthRejectedPodUID = string(pod.UID)
	if err := k8s.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent = getAgent(t, k8s, client.ObjectKeyFromObject(agent))
	event, err := r.classifyEvent(ctx, agent, pod)
	if err != nil || event != EventInferenceAuthRejected {
		t.Fatalf("event=%q err=%v", event, err)
	}
	result, err := NextPhase(agent.Status.Phase, event)
	if err != nil || result.NextPhase != kyberv1.AgentPhaseNeedsAuth || result.Action != ActionCaptureStateAndDeletePod {
		t.Fatalf("transition=%+v err=%v", result, err)
	}
	if _, err := r.executeAction(ctx, agent, pod, result.Action, event); err != nil {
		t.Fatal(err)
	}
	if err := r.updatePhase(ctx, agent, result.NextPhase, ""); err != nil {
		t.Fatal(err)
	}
	got := getAgent(t, k8s, client.ObjectKeyFromObject(agent))
	if got.Status.Phase != kyberv1.AgentPhaseNeedsAuth {
		t.Fatalf("phase=%s", got.Status.Phase)
	}
	if want := "rv:auth-inference:" + secret.ResourceVersion; got.Status.RecoveryInput != want {
		t.Fatalf("recovery input=%q, want %q", got.Status.RecoveryInput, want)
	}
	if got.Status.Activity.InferenceAuthRejectedPodUID != "" {
		t.Fatal("signal was not consumed")
	}
}
