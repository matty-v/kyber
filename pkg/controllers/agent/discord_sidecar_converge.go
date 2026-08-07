package agent

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/tokenreport"
)

func isDiscordSidecarDrifted(pod *corev1.Pod, enabled bool, revision string) bool {
	if pod == nil {
		return false
	}
	has := extractContainerSpecImage(pod, DiscordSidecarContainerName) != ""
	if has != enabled {
		return true
	}
	if !enabled {
		return false
	}
	return pod.Annotations[DiscordConfigRevisionAnnotation] != revision
}

// convergeDiscordSidecar applies presence and configuration changes once the
// runtime reports Idle. The shared delete budget prevents simultaneous agent
// disruption; a working or unknown agent keeps its current session until idle.
func (r *AgentReconciler) convergeDiscordSidecar(ctx context.Context, agent *kyberv1.Agent, pod *corev1.Pod) (bool, error) {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false, nil
	}
	enabled := agent.Spec.Channels != nil && agent.Spec.Channels.Discord != nil
	revision := agent.Annotations[DiscordConfigRevisionAnnotation]
	if !isDiscordSidecarDrifted(pod, enabled, revision) {
		return false, nil
	}
	if enabled && r.DiscordSidecarImage == "" {
		return false, nil
	}
	if agent.Status.Activity == nil || agent.Status.Activity.State != tokenreport.ActivityIdle {
		return false, nil
	}
	inflight, err := r.countAgentPodsBeingDeleted(ctx, agent.Namespace)
	if err != nil {
		return false, fmt.Errorf("counting in-flight pod deletions: %w", err)
	}
	if inflight >= sidecarAutoRollDefaultMaxConcurrent {
		return false, nil
	}
	if err := r.Delete(ctx, pod); err != nil {
		return false, fmt.Errorf("deleting Discord-drifted pod: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Event(agent, corev1.EventTypeNormal, "DiscordConfigConverging", "Rolling idle agent pod to apply Discord configuration")
	}
	return true, nil
}
