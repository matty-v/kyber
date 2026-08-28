package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// A runtime that cannot authenticate must land in NeedsAuth (which lights up
// the PWA's Re-authorize button), not in a generic auto-restart loop against
// the same broken credential. Each runtime's start script signals this with its
// own exit code: 2 from start-claude.sh, 42 from start-codex.sh.
func TestIsOAuthRefreshFailure_ExitCodes(t *testing.T) {
	podWith := func(container string, exit int32, terminated bool) *corev1.Pod {
		st := corev1.ContainerStatus{Name: container}
		if terminated {
			st.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: exit}
		} else {
			st.State.Running = &corev1.ContainerStateRunning{}
		}
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{st}}}
	}

	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"claude-code auth failure (exit 2)", podWith("agent", 2, true), true},
		{"codex auth failure (exit 42)", podWith("agent", 42, true), true},
		{"generic crash (exit 1)", podWith("agent", 1, true), false},
		{"OOM kill (exit 137)", podWith("agent", 137, true), false},
		{"clean exit", podWith("agent", 0, true), false},
		{"still running", podWith("agent", 0, false), false},
		{"exit 42 in a sidecar, not the agent", podWith("kyber-status-sidecar", 42, true), false},
		{"nil pod", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOAuthRefreshFailure(tc.pod); got != tc.want {
				t.Fatalf("isOAuthRefreshFailure = %v, want %v", got, tc.want)
			}
		})
	}
}

// LastTerminationState must stay out of it: an earlier auth failure followed by
// a different crash is not a credential problem now.
func TestIsOAuthRefreshFailure_IgnoresLastTerminationState(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:                 "agent",
		State:                corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 42}},
	}}}}
	if isOAuthRefreshFailure(pod) {
		t.Fatal("a prior exit-42 must not classify a current exit-1 crash as a credential failure")
	}
}

func TestRuntimeProbeFailureIsDistinctFromAuthentication(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "agent",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 43}},
	}}}}
	if !isRuntimeProbeFailure(pod) {
		t.Fatal("exit 43 must classify as a runtime probe failure")
	}
	if isOAuthRefreshFailure(pod) {
		t.Fatal("exit 43 must not classify as an authentication failure")
	}
}

func TestBrokenRuntimeSuppressesForcedAuthenticationTransition(t *testing.T) {
	agent := &kyberv1.Agent{}
	agent.Status.Phase = kyberv1.AgentPhaseBrokenRuntime
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseNeedsAuth
	event, err := (&AgentReconciler{}).classifyEvent(context.Background(), agent, nil)
	if err != nil || event != "" {
		t.Fatalf("BrokenRuntime force-auth classification = %q, err=%v; want stable", event, err)
	}
}
