package agent

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestSlackSidecarDrift(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		enabled  bool
		revision string
		want     bool
	}{
		{"matching", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{SlackConfigRevisionAnnotation: "new"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: SlackSidecarContainerName, Image: "slack:v1"}}}}, true, "new", false},
		{"changed revision", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{SlackConfigRevisionAnnotation: "old"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: SlackSidecarContainerName, Image: "slack:v1"}}}}, true, "new", true},
		{"missing enabled sidecar", &corev1.Pod{}, true, "new", true},
		{"stale disabled sidecar", &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: SlackSidecarContainerName, Image: "slack:v1"}}}}, false, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSlackSidecarDrifted(tc.pod, tc.enabled, tc.revision); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
