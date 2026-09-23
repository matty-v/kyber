package agent

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// archiveHold returns the disk export/import job that currently owns the
// agent's volume, or "" (MAT-87/MAT-88).
func archiveHold(agent *kyberv1.Agent) string {
	return agent.Annotations[kyberv1.AnnotationArchiveHold]
}

// awaitingRestore reports whether the agent's first pod is being held back
// while its volume is restored from an archive.
func awaitingRestore(agent *kyberv1.Agent) bool {
	return meta.IsStatusConditionTrue(agent.Status.Conditions, kyberv1.AgentConditionAwaitingRestore)
}

// markAwaitingRestore moves a never-started agent to Creating and records why
// it has no pod. Patches only when something changed.
func (r *AgentReconciler) markAwaitingRestore(ctx context.Context, agent *kyberv1.Agent) error {
	patch := client.MergeFrom(agent.DeepCopy())
	changed := false
	if agent.Status.Phase == "" {
		agent.Status.Phase = kyberv1.AgentPhaseCreating
		now := metav1.Now()
		agent.Status.LastTransition = &now
		changed = true
	}
	if meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:    kyberv1.AgentConditionAwaitingRestore,
		Status:  metav1.ConditionTrue,
		Reason:  "Restoring",
		Message: "Restoring the agent's volume from disk archive job " + archiveHold(agent) + ".",
	}) {
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Status().Patch(ctx, agent, patch)
}
