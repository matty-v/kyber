package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestA2AConfigRevisionDetectsPeerChanges(t *testing.T) {
	peers := []kyberv1.AgentA2APeer{{Name: "auditor", URL: "https://agents.example/auditor"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{A2AConfigRevisionAnnotation: a2aConfigRevision(peers)}}}
	if isA2AConfigDrifted(pod, peers) {
		t.Fatal("matching peer configuration reported drift")
	}
	peers[0].URL = "https://agents.example/new-auditor"
	if !isA2AConfigDrifted(pod, peers) {
		t.Fatal("changed peer configuration did not report drift")
	}
}

func TestA2AConfigRevisionDoesNotRollLegacyPodWithoutPeers(t *testing.T) {
	if isA2AConfigDrifted(&corev1.Pod{}, nil) {
		t.Fatal("legacy pod without peers reported drift")
	}
}

func TestConvergeA2AConfigRequestsIntentionalRestart(t *testing.T) {
	agent := idleAgent("coordinator", "kyber")
	agent.Spec.A2APeers = []kyberv1.AgentA2APeer{{Name: "auditor", URL: "https://agents.example/auditor"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: AgentPodName(agent.Name), Namespace: agent.Namespace, Labels: map[string]string{"kyber.io/agent": agent.Name}, Annotations: map[string]string{A2AConfigRevisionAnnotation: a2aConfigRevision(nil)}}}
	r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).WithObjects(agent, pod).Build()}
	rolled, err := r.convergeA2AConfig(context.Background(), agent, pod)
	if err != nil {
		t.Fatalf("convergeA2AConfig: %v", err)
	}
	if !rolled {
		t.Fatal("convergeA2AConfig did not request a rollout")
	}
	got := &kyberv1.Agent{}
	if err := r.Get(context.Background(), clientKey(agent), got); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Spec.DesiredPhase != kyberv1.AgentPhaseRestarting {
		t.Fatalf("desired phase = %q, want Restarting", got.Spec.DesiredPhase)
	}
}

func clientKey(agent *kyberv1.Agent) client.ObjectKey {
	return client.ObjectKeyFromObject(agent)
}
