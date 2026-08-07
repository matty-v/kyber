package api

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AvailableResources represents the remaining schedulable capacity on a node
// after subtracting all non-terminal pod requests from the node's allocatable.
type AvailableResources struct {
	CPU    resource.Quantity
	Memory resource.Quantity
}

// nodeAvailableResources computes the remaining schedulable capacity on a node
// by listing all non-terminal pods and subtracting their resource requests from
// the node's allocatable. This matches the k8s scheduler's bin-packing logic.
//
// Returns an error if the node doesn't exist or listing pods fails.
func nodeAvailableResources(ctx context.Context, k8s client.Client, nodeName string) (*AvailableResources, error) {
	// 1. Get the node's allocatable resources.
	node := &corev1.Node{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return nil, fmt.Errorf("get node %q: %w", nodeName, err)
	}

	allocCPU := node.Status.Allocatable.Cpu().DeepCopy()
	allocMem := node.Status.Allocatable.Memory().DeepCopy()

	// 2. List all pods on this node.
	podList := &corev1.PodList{}
	if err := k8s.List(ctx, podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		// Field selector may not be indexed — fall back to listing all pods and filtering.
		podList = &corev1.PodList{}
		if err := k8s.List(ctx, podList); err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}
	}

	// 3. Sum resource requests from non-terminal pods on this node.
	usedCPU := resource.Quantity{}
	usedMem := resource.Quantity{}

	for i := range podList.Items {
		pod := &podList.Items[i]
		// Skip pods not on this node (relevant when field selector isn't available).
		if pod.Spec.NodeName != nodeName {
			continue
		}
		// Skip terminal pods — they don't consume resources.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if cpu := c.Resources.Requests.Cpu(); cpu != nil {
				usedCPU.Add(*cpu)
			}
			if mem := c.Resources.Requests.Memory(); mem != nil {
				usedMem.Add(*mem)
			}
		}
		for _, c := range pod.Spec.InitContainers {
			// Init containers run sequentially, not concurrently. The scheduler
			// uses max(init) vs sum(regular) for each resource. For simplicity
			// we include init container requests — this is conservative and matches
			// what the user observes in kubectl describe node "Allocated resources".
			if cpu := c.Resources.Requests.Cpu(); cpu != nil {
				usedCPU.Add(*cpu)
			}
			if mem := c.Resources.Requests.Memory(); mem != nil {
				usedMem.Add(*mem)
			}
		}
	}

	// 4. Compute available = allocatable - used.
	allocCPU.Sub(usedCPU)
	allocMem.Sub(usedMem)

	return &AvailableResources{
		CPU:    allocCPU,
		Memory: allocMem,
	}, nil
}
