package machine

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

const (
	capacityRequestLabel = "kyber.io/capacity-request"
	capacityRequestImage = "registry.k8s.io/pause:3.10"
)

func capacityRequestPodName(machineName string) string {
	return machineName + "-capacity-request"
}

// reconcileCapacityRequestPod keeps exactly one minimal, unschedulable Pod
// while a scheduler-driven provider has active Agent demand but no Ready Node.
// The Pod carries no credentials and exists only to make provider autoscaling
// observe the demand that WaitingForMachine intentionally removes.
func (r *MachineReconciler) reconcileCapacityRequestPod(ctx context.Context, machine *kyberv1.Machine) error {
	key := types.NamespacedName{Name: capacityRequestPodName(machine.Name), Namespace: machine.Namespace}
	wanted, selector, err := r.capacityRequestWanted(ctx, machine)
	if err != nil {
		return err
	}

	existing := &corev1.Pod{}
	err = r.Get(ctx, key, existing)
	if !wanted {
		return r.deleteCapacityRequestPod(ctx, existing, err)
	}
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting capacity request pod: %w", err)
	}

	no := false
	zero := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			Labels: map[string]string{capacityRequestLabel: "true"},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &zero,
			NodeSelector:                  selector,
			Containers: []corev1.Container{{
				Name: "capacity-request", Image: capacityRequestImage,
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1m"),
					corev1.ResourceMemory: resource.MustParse("1Mi"),
				}},
			}},
		},
	}
	if err := ctrl.SetControllerReference(machine, pod, r.Scheme); err != nil {
		return fmt.Errorf("setting capacity request owner: %w", err)
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating capacity request pod: %w", err)
	}
	r.Recorder.Eventf(machine, corev1.EventTypeNormal, "CapacityRequested",
		"Active Agent demand is requesting provider capacity")
	return nil
}

func (r *MachineReconciler) deleteCapacityRequestPod(
	ctx context.Context,
	existing *corev1.Pod,
	getErr error,
) error {
	if apierrors.IsNotFound(getErr) {
		return nil
	}
	if getErr != nil {
		return fmt.Errorf("getting capacity request pod: %w", getErr)
	}
	if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting capacity request pod: %w", err)
	}
	return nil
}

func (r *MachineReconciler) removeCapacityRequestPod(ctx context.Context, machine *kyberv1.Machine) error {
	key := types.NamespacedName{Name: capacityRequestPodName(machine.Name), Namespace: machine.Namespace}
	existing := &corev1.Pod{}
	err := r.Get(ctx, key, existing)
	return r.deleteCapacityRequestPod(ctx, existing, err)
}

func (r *MachineReconciler) capacityRequestWanted(ctx context.Context, machine *kyberv1.Machine) (bool, map[string]string, error) {
	if machine.Spec.ManagementMode == kyberv1.MachineManagementExternal ||
		machine.Spec.DesiredPhase != kyberv1.MachinePhaseRunning ||
		(machine.Status.Phase != kyberv1.MachinePhaseProvisioning &&
			machine.Status.Phase != kyberv1.MachinePhasePreempted &&
			machine.Status.Phase != kyberv1.MachinePhaseReplacing) {
		return false, nil, nil
	}
	provider := r.capacityProviderFor(machine)
	demandProvider, ok := provider.(adapters.CapacityNeedsSchedulerDemand)
	if !ok || !demandProvider.NeedsSchedulerDemand() {
		return false, nil, nil
	}
	selectorProvider, ok := provider.(adapters.CapacityNodeSelector)
	if !ok {
		return false, nil, fmt.Errorf("scheduler-demand provider %q has no node selector", machine.Spec.Provider)
	}
	selector := selectorProvider.NodeSelector(
		adapters.MachineIdentity{Name: machine.Name},
		adapters.ProviderRef(providerReference(machine)),
	)
	if len(selector) == 0 {
		return false, nil, fmt.Errorf("scheduler-demand provider %q returned an empty node selector", machine.Spec.Provider)
	}

	agents := &kyberv1.AgentList{}
	if err := r.List(ctx, agents, client.InNamespace(machine.Namespace)); err != nil {
		return false, nil, fmt.Errorf("listing Agent demand: %w", err)
	}
	for i := range agents.Items {
		agent := &agents.Items[i]
		if agent.Spec.Machine == machine.Name && agentNeedsMachineCapacity(agent) {
			return true, selector, nil
		}
	}
	return false, selector, nil
}

func agentNeedsMachineCapacity(agent *kyberv1.Agent) bool {
	switch agent.Spec.DesiredPhase {
	case kyberv1.AgentPhaseStopped:
		return false
	case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseRestarting, kyberv1.AgentPhaseNeedsAuth:
		return true
	}
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseStopped,
		kyberv1.AgentPhaseDeleted, kyberv1.AgentPhaseFailed,
		kyberv1.AgentPhaseMemoryExhausted:
		return false
	default:
		return true
	}
}
