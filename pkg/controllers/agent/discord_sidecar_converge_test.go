package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIsDiscordSidecarDrifted(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{DiscordConfigRevisionAnnotation: "old"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: DiscordSidecarContainerName, Image: "discord:v1"}}}}
	if !isDiscordSidecarDrifted(pod, true, "new") {
		t.Fatal("changed Discord config revision must be drift")
	}
	if !isDiscordSidecarDrifted(pod, false, "") {
		t.Fatal("disabled Discord with a live sidecar must be drift")
	}
	if isDiscordSidecarDrifted(pod, true, "old") {
		t.Fatal("matching image and revision must converge")
	}
}

func TestConvergeDiscordSidecar_RollsIdlePod(t *testing.T) {
	ag := idleAgent("dave", "kyber")
	ag.Spec.Channels = discordAgent(t, "action", false, true).Spec.Channels
	ag.Annotations = map[string]string{DiscordConfigRevisionAnnotation: "new"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: AgentPodName("dave"), Namespace: "kyber", Labels: map[string]string{"kyber.io/agent": "dave"}, Annotations: map[string]string{DiscordConfigRevisionAnnotation: "old"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: AgentContainerName}, {Name: DiscordSidecarContainerName, Image: "discord:v1"}}}}
	r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).WithObjects(ag, pod).Build(), DiscordSidecarImage: "discord:v1"}
	rolled, err := r.convergeDiscordSidecar(context.Background(), ag, pod)
	if err != nil || !rolled {
		t.Fatalf("rolled=%v err=%v", rolled, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err == nil {
		t.Fatal("stale Discord pod was not deleted")
	}
}

func TestConvergeDiscordSidecar_EnvtestDeletesOnlyWhenIdle(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-discord-converge"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	ag := newTestAgent("dave", ns.Name)
	ag.Spec.Channels = discordAgent(t, "action", false, true).Spec.Channels
	ag.Annotations = map[string]string{DiscordConfigRevisionAnnotation: "new"}
	if err := k8sClient.Create(ctx, ag); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: AgentPodName(ag.Name), Namespace: ns.Name,
		Labels:      map[string]string{"kyber.io/agent": ag.Name},
		Annotations: map[string]string{DiscordConfigRevisionAnnotation: "old"},
	}, Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: AgentContainerName, Image: "runtime:test"},
		{Name: DiscordSidecarContainerName, Image: "discord:test"},
	}}}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("creating pod: %v", err)
	}
	r := &AgentReconciler{Client: k8sClient, DiscordSidecarImage: "discord:test"}

	ag.Status.Activity = nil
	rolled, err := r.convergeDiscordSidecar(ctx, ag, pod)
	if err != nil || rolled {
		t.Fatalf("unknown activity rolled=%v err=%v", rolled, err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("working/unknown pod was changed: %v", err)
	}

	ag.Status.Activity = idleAgent("dave", ns.Name).Status.Activity
	rolled, err = r.convergeDiscordSidecar(ctx, ag, pod)
	if err != nil || !rolled {
		t.Fatalf("idle rolled=%v err=%v", rolled, err)
	}
	err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("idle stale pod still exists: %v", err)
	}
}
