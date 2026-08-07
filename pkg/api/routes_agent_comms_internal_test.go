package api

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/matty-v/kyber/pkg/controllers/agent"
)

func TestDiscordConnectionState(t *testing.T) {
	cases := []struct {
		name            string
		configured      bool
		restartRequired bool
		pod             *corev1.Pod
		wantStatus      string
		wantReady       bool
		wantRestarts    int32
	}{
		{name: "not configured", wantStatus: "not-configured"},
		{name: "restart required", configured: true, restartRequired: true, wantStatus: "restart-required"},
		{name: "agent stopped", configured: true, wantStatus: "not-running"},
		{name: "connected", configured: true, pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: agent.DiscordSidecarContainerName, Ready: true, RestartCount: 2}}}}, wantStatus: "connected", wantReady: true, wantRestarts: 2},
		{name: "crash loop", configured: true, pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: agent.DiscordSidecarContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}, wantStatus: "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := discordConnectionState(tc.configured, tc.restartRequired, tc.pod)
			if got.Status != tc.wantStatus || got.Ready != tc.wantReady || got.RestartCount != tc.wantRestarts {
				t.Fatalf("state = %+v, want status=%q ready=%v restarts=%d", got, tc.wantStatus, tc.wantReady, tc.wantRestarts)
			}
		})
	}
}
