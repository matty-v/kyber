package agent

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// SetupWithManager registers the AgentReconciler with the controller-runtime Manager.
// It watches:
//   - Agent CRDs (create, update, delete)
//   - Pods owned by Agent CRDs (status changes trigger reconciliation of the owning Agent)
//   - PVCs owned by Agent CRDs
//
// A periodic resync is configured on the Manager (every 5 minutes), catching any drift
// between desired and actual state.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.MachineGetter == nil {
		return fmt.Errorf("setting up agent controller: machine getter is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kyberv1.Agent{}).
		// Watch pods we own: any pod status change triggers reconciliation of the owning Agent.
		Owns(&corev1.Pod{}, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		// Watch PVCs we own (e.g., for deletion propagation).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
