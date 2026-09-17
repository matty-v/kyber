package agent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

func TestClassifyRuntimeFailureUsesRuntimeOwnedCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int32
		want runtimeFailure
	}{
		{name: "confirmed auth", code: 2, want: runtimeFailureAuthentication},
		{name: "provider service", code: 44, want: runtimeFailureAuthService},
		{name: "credential sync", code: 45, want: runtimeFailureCredentialSync},
		{name: "generic crash", code: 1, want: runtimeFailureNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := terminatedClaudePod(tc.code)
			if got := classifyRuntimeFailure(pod, "codex"); got != tc.want {
				t.Fatalf("classification = %v, want %v (pod runtime label must beat changed spec)", got, tc.want)
			}
		})
	}
}

func TestReconcilerAuthServiceFailureUsesBoundedRecovery(t *testing.T) {
	k8sClient, teardown := setupEnvtest(t)
	defer teardown()

	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-auth-service-recovery"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	agent := newTestAgent("service-failure", ns.Name)
	agent.Spec.Runtime = "claude-code"
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("creating agent: %v", err)
	}
	r := newReconciler(k8sClient, buildTestScheme())
	key := types.NamespacedName{Name: agent.Name, Namespace: ns.Name}
	req := ctrl.Request{NamespacedName: key}
	reconcileN(t, r, req, 1)

	current := getAgent(t, k8sClient, key)
	statusPatch := client.MergeFrom(current.DeepCopy())
	current.Status.Phase = kyberv1.AgentPhaseRunning
	now := metav1.Now()
	current.Status.LastTransition = &now
	current.Status.StartTime = &now
	if err := k8sClient.Status().Patch(ctx, current, statusPatch); err != nil {
		t.Fatalf("setting Running status: %v", err)
	}
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: AgentPodName(agent.Name), Namespace: ns.Name}, pod); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	metadataPatch := client.MergeFrom(pod.DeepCopy())
	pod.Labels["kyber.io/runtime"] = "claude-code"
	if err := k8sClient.Patch(ctx, pod, metadataPatch); err != nil {
		t.Fatalf("setting launched runtime label: %v", err)
	}
	podPatch := client.MergeFrom(pod.DeepCopy())
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = terminatedClaudePod(44).Status.ContainerStatuses
	if err := k8sClient.Status().Patch(ctx, pod, podPatch); err != nil {
		t.Fatalf("setting terminal pod status: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconciling service failure: %v", err)
	}
	updated := getAgent(t, k8sClient, key)
	if updated.Status.Phase != kyberv1.AgentPhaseFailed || updated.Status.RestartCount != 1 {
		t.Fatalf("status = phase:%s restartCount:%d, want Failed/1", updated.Status.Phase, updated.Status.RestartCount)
	}
	if !strings.Contains(updated.Status.Message, "provider or network is unavailable") || strings.Contains(updated.Status.Message, "reauthor") {
		t.Fatalf("status message is not actionable for a service failure: %q", updated.Status.Message)
	}
	originalMessage := updated.Status.Message
	retryPatch := client.MergeFrom(updated.DeepCopy())
	updated.Status.RestartCount = maxRestartRetries
	if err := k8sClient.Status().Patch(ctx, updated, retryPatch); err != nil {
		t.Fatalf("setting exhausted retry count: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconciling exhausted service failure: %v", err)
	}
	updated = getAgent(t, k8sClient, key)
	if updated.Status.Message != originalMessage {
		t.Fatalf("retry-limit reconcile cleared actionable message: got %q, want %q", updated.Status.Message, originalMessage)
	}
}

func TestClassifyRuntimeFailureIgnoresSidecarsAndOldTerminations(t *testing.T) {
	pod := terminatedClaudePod(1)
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses,
		corev1.ContainerStatus{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 44}}})
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 45}
	if got := classifyRuntimeFailure(pod, "claude-code"); got != runtimeFailureNone {
		t.Fatalf("classification = %v, want none", got)
	}
}

func TestClassifyEventRoutesClaudeFailureCategories(t *testing.T) {
	r := &AgentReconciler{}
	for _, phase := range []kyberv1.AgentPhase{kyberv1.AgentPhaseStarting, kyberv1.AgentPhaseRunning} {
		for code, want := range map[int32]Event{2: EventOAuthRefreshFailed, 44: EventAuthServiceFailed, 45: EventCredentialSyncFailed} {
			agent := &kyberv1.Agent{Spec: kyberv1.AgentSpec{Runtime: "claude-code"}, Status: kyberv1.AgentStatus{Phase: phase}}
			event, err := r.classifyEvent(context.Background(), agent, terminatedClaudePod(code))
			if err != nil {
				t.Fatalf("phase %s code %d: %v", phase, code, err)
			}
			if event != want {
				t.Fatalf("phase %s code %d: event = %s, want %s", phase, code, event, want)
			}
		}
	}
}

func terminatedClaudePod(exitCode int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"kyber.io/runtime": "claude-code"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  AgentContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
			}},
		},
	}
}
