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
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-inference", Namespace: ns.Name},
		Data:       map[string][]byte{"token": []byte("rejected")},
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	rejectedRV := secret.ResourceVersion
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
		ObjectMeta: metav1.ObjectMeta{
			Name: AgentPodName(agent.Name), Namespace: ns.Name,
			Annotations: map[string]string{kyberv1.AgentInferenceCredentialRVAnnotation: rejectedRV},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName, Image: "runtime:test"}}},
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
	agent.Status.Activity.InferenceAuthRejectedSecretRV = rejectedRV
	if err := k8s.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent = getAgent(t, k8s, client.ObjectKeyFromObject(agent))
	event, err := r.classifyEvent(ctx, agent, pod)
	if err != nil || event != EventInferenceAuthRejected {
		t.Fatalf("event=%q err=%v", event, err)
	}
	// A replacement key arriving after classification must remain eligible
	// for recovery; the failed pod used rejectedRV, not the latest version.
	secret.Data["token"] = []byte("replacement")
	if err := k8s.Update(ctx, secret); err != nil {
		t.Fatal(err)
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
	if want := "rv:auth-inference:" + rejectedRV; got.Status.RecoveryInput != want {
		t.Fatalf("recovery input=%q, want %q", got.Status.RecoveryInput, want)
	}
	if secret.ResourceVersion == rejectedRV {
		t.Fatal("precondition: Secret did not rotate")
	}
	if got.Status.Activity.InferenceAuthRejectedPodUID != "" {
		t.Fatal("signal was not consumed")
	}
}

func TestInferenceCredentialRotated_Envtest(t *testing.T) {
	k8s, teardown := setupEnvtest(t)
	defer teardown()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-inference-rotation"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rotate-inference", Namespace: ns.Name},
		Data:       map[string][]byte{"token": []byte("old")},
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	oldRV := secret.ResourceVersion
	agent := newTestAgent("rotate", ns.Name)
	agent.Spec.Runtime = "codex"
	agent.Spec.Inference = &kyberv1.AgentInference{
		BaseURL: "https://example.com/v1", API: "openai",
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: secret.Name, Key: "token"},
	}
	if err := k8s.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: AgentPodName(agent.Name), Namespace: ns.Name,
			Annotations: map[string]string{kyberv1.AgentInferenceCredentialRVAnnotation: oldRV},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName, Image: "runtime:test"}}},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	agent = getAgent(t, k8s, client.ObjectKeyFromObject(agent))
	agent.Status.Phase = kyberv1.AgentPhaseRunning
	if err := k8s.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent = getAgent(t, k8s, client.ObjectKeyFromObject(agent))
	r := newReconciler(k8s, k8s.Scheme())
	r.AdapterRegistry = map[string]pkgruntimes.Adapter{"codex": codex.NewAdapter()}
	if event, err := r.classifyEvent(ctx, agent, pod); err != nil || event != "" {
		t.Fatalf("unchanged Secret event=%q err=%v", event, err)
	}
	secret.Data["token"] = []byte("replacement")
	if err := k8s.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	event, err := r.classifyEvent(ctx, agent, pod)
	if err != nil || event != EventInferenceCredentialRotated {
		t.Fatalf("event=%q err=%v", event, err)
	}
	result, err := NextPhase(agent.Status.Phase, event)
	if err != nil || result.NextPhase != kyberv1.AgentPhaseRestarting || result.Action != ActionCaptureStateAndDeletePod {
		t.Fatalf("transition=%+v err=%v", result, err)
	}
	if _, err := r.executeAction(ctx, agent, pod, result.Action, event); err != nil {
		t.Fatal(err)
	}
	if err := r.updatePhase(ctx, agent, result.NextPhase, ""); err != nil {
		t.Fatal(err)
	}
	got := getAgent(t, k8s, client.ObjectKeyFromObject(agent))
	if got.Status.Phase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("phase=%s", got.Status.Phase)
	}
}

func TestStampInferenceCredentialVersion(t *testing.T) {
	agent := needsAuthAgent("")
	name := rigAgent + "-inference"
	agent.Spec.Inference = &kyberv1.AgentInference{
		Credential: kyberv1.AgentInferenceCredentialRef{ExistingSecret: name, Key: "token"},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: rigNS, ResourceVersion: "200",
	}}
	r := newGateReconciler(t, secret)
	pod := &corev1.Pod{}
	if err := r.stampInferenceCredentialVersion(context.Background(), agent, pod); err != nil {
		t.Fatal(err)
	}
	if got := pod.Annotations[kyberv1.AgentInferenceCredentialRVAnnotation]; got != "200" {
		t.Fatalf("stamped version=%q, want 200", got)
	}
}
