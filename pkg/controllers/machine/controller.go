package machine

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ctrl "sigs.k8s.io/controller-runtime"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// SetupWithManager registers the MachineReconciler with the controller-runtime Manager.
// It watches:
//   - Machine CRDs (create, update, delete)
//   - k8s Nodes (status changes may indicate the machine's node joined or was lost)
//
// A periodic resync is configured on the Manager (every 2 minutes per spec), catching
// any drift between desired machine state and actual VM/node state. The 2-minute cadence
// is time-sensitive for preemption detection.
//
// Also registers a field indexer on Pod.spec.nodeName so countAgentPods can use
// client.MatchingFields{"spec.nodeName": ...}. Without this, the list call fails with
// "Index with name field:spec.nodeName does not exist" and the reconciler loops
// erroring out on every Machine that has reached Ready.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1.Pod{},
		"spec.nodeName",
		func(obj client.Object) []string {
			p, ok := obj.(*corev1.Pod)
			if !ok || p.Spec.NodeName == "" {
				return nil
			}
			return []string{p.Spec.NodeName}
		},
	); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kyberv1.Machine{}).
		// Watch nodes: node status changes (Ready, NotReady, deletion) trigger
		// reconciliation of all machines in the same namespace, so the controller
		// can detect node join after provisioning and preemption after node loss.
		// Nodes are not owned by Machine CRDs (they are created by the VM startup
		// script), so we use a fan-out handler rather than Owns().
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAllMachines)).
		Complete(r)
}

// enqueueAllMachines is called when any k8s Node changes. It enqueues reconcile
// requests for all Machine CRDs across all namespaces so each Machine can re-check
// whether its node has joined, become Ready, or disappeared.
func (r *MachineReconciler) enqueueAllMachines(ctx context.Context, _ client.Object) []reconcile.Request {
	machineList := &kyberv1.MachineList{}
	if err := r.List(ctx, machineList); err != nil {
		// Log error but don't block — the periodic resync will catch it.
		return nil
	}

	requests := make([]reconcile.Request, 0, len(machineList.Items))
	for _, m := range machineList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      m.Name,
				Namespace: m.Namespace,
			},
		})
	}
	return requests
}
