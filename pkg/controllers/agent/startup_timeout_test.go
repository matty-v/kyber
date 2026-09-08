package agent

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestStartupTimeoutForPodHonorsLongerLivenessBudget(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: AgentContainerName,
		LivenessProbe: &corev1.Probe{
			InitialDelaySeconds: 300,
			PeriodSeconds:       30,
			FailureThreshold:    3,
		},
	}}}}

	if got, want := startupTimeoutForPod(pod), 390*time.Second; got != want {
		t.Fatalf("startupTimeoutForPod() = %v, want %v", got, want)
	}
}

func TestStartupTimeoutForPodKeepsDefaultFloor(t *testing.T) {
	for name, pod := range map[string]*corev1.Pod{
		"nil pod": nil,
		"no probe": &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: AgentContainerName}},
		}},
		"short probe": &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: AgentContainerName,
				LivenessProbe: &corev1.Probe{
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
					FailureThreshold:    3,
				},
			}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := startupTimeoutForPod(pod); got != startupTimeoutSeconds {
				t.Fatalf("startupTimeoutForPod() = %v, want %v", got, startupTimeoutSeconds)
			}
		})
	}
}
