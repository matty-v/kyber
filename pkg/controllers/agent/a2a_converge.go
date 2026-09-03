package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/tokenreport"
)

const A2AConfigRevisionAnnotation = "kyber.io/a2a-config-revision"

func a2aConfigRevision(peers []kyberv1.AgentA2APeer) string {
	encoded, _ := json.Marshal(peers)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func isA2AConfigDrifted(pod *corev1.Pod, peers []kyberv1.AgentA2APeer) bool {
	if pod == nil {
		return false
	}
	// Pods created before outbound A2A had no revision annotation. Treat that
	// legacy state as converged when the Agent also has no peers, avoiding a
	// fleet-wide no-op rollout on upgrade. A pod that previously had peers has
	// an annotation and still rolls when the final peer is removed.
	if len(peers) == 0 && pod.Annotations[A2AConfigRevisionAnnotation] == "" {
		return false
	}
	return pod.Annotations[A2AConfigRevisionAnnotation] != a2aConfigRevision(peers)
}

// convergeA2AConfig rolls an idle agent through the normal intentional-restart
// path when its operator-curated peer set changes. The peer configuration is
// injected as sidecar environment at pod creation, so an existing pod cannot
// observe a CR update without recreation.
func (r *AgentReconciler) convergeA2AConfig(ctx context.Context, agent *kyberv1.Agent, pod *corev1.Pod) (bool, error) {
	if pod == nil || pod.DeletionTimestamp != nil || !isA2AConfigDrifted(pod, agent.Spec.A2APeers) {
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
	requested, err := r.requestIntentionalRestart(ctx, agent)
	if err != nil {
		return false, fmt.Errorf("requesting restart for A2A-config-drifted pod: %w", err)
	}
	if !requested {
		return false, nil
	}
	if r.Recorder != nil {
		r.Recorder.Event(agent, corev1.EventTypeNormal, "A2AConfigConverging", "Rolling idle agent pod to apply outbound A2A peer configuration")
	}
	return true, nil
}
