package agent

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// JobsProjectionProbe reads only the revision marker from the agent's mounted
// ConfigMap. Kubernetes projects all keys through one atomic symlink switch,
// so this marker acknowledges the crontab and prompt files together.
type JobsProjectionProbe func(ctx context.Context, pod *corev1.Pod) (string, error)

func NewKubernetesJobsProjectionProbe(config *rest.Config, clientset kubernetes.Interface) JobsProjectionProbe {
	return func(ctx context.Context, pod *corev1.Pod) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").Name(pod.Name).Namespace(pod.Namespace).
			SubResource("exec").
			Param("container", AgentContainerName).
			Param("stdout", "true").Param("stderr", "true").
			Param("command", "cat").Param("command", JobsSourceMountPath+"/revision")
		executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			return "", fmt.Errorf("opening jobs projection probe: %w", err)
		}
		var stdout, stderr bytes.Buffer
		if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: &stdout, Stderr: &stderr,
		}); err != nil {
			return "", fmt.Errorf("reading jobs projection marker: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
}
