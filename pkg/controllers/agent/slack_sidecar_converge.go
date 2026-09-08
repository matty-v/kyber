package agent

import (
	"context"
	"fmt"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/tokenreport"
	corev1 "k8s.io/api/core/v1"
)

func isSlackSidecarDrifted(pod *corev1.Pod, enabled bool, revision string) bool {
	if pod == nil {
		return false
	}
	has := extractContainerSpecImage(pod, SlackSidecarContainerName) != ""
	if has != enabled {
		return true
	}
	if !enabled {
		return false
	}
	return pod.Annotations[SlackConfigRevisionAnnotation] != revision
}

func (r *AgentReconciler) convergeSlackSidecar(ctx context.Context, a *kyberv1.Agent, pod *corev1.Pod) (bool, error) {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false, nil
	}
	enabled := a.Spec.Secrets.SlackEnabled
	revision := a.Annotations[SlackConfigRevisionAnnotation]
	if !isSlackSidecarDrifted(pod, enabled, revision) || (enabled && r.SlackSidecarImage == "") {
		return false, nil
	}
	if a.Status.Activity == nil || a.Status.Activity.State != tokenreport.ActivityIdle {
		return false, nil
	}
	inflight, err := r.countAgentPodsBeingDeleted(ctx, a.Namespace)
	if err != nil {
		return false, fmt.Errorf("counting in-flight pod deletions: %w", err)
	}
	if inflight >= sidecarAutoRollDefaultMaxConcurrent {
		return false, nil
	}
	requested, err := r.requestIntentionalRestart(ctx, a)
	if err != nil {
		return false, fmt.Errorf("requesting restart for Slack-drifted pod: %w", err)
	}
	if !requested {
		return false, nil
	}
	if r.Recorder != nil {
		r.Recorder.Event(a, corev1.EventTypeNormal, "SlackConfigConverging", "Rolling idle agent pod to apply Slack configuration")
	}
	return true, nil
}
