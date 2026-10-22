package api

import (
	"context"
	"fmt"
	"log/slog"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type agentAdmissionPolicy struct {
	limits corev1.LimitRangeList
	quotas corev1.ResourceQuotaList
}

var agentPodLimitOverheadCPU = resource.MustParse("600m")
var agentPodLimitOverheadMemory = resource.MustParse("512Mi")

func loadAgentAdmissionPolicy(ctx context.Context, reader client.Reader, namespace string) (*agentAdmissionPolicy, error) {
	policy := &agentAdmissionPolicy{}
	if err := reader.List(ctx, &policy.limits, client.InNamespace(namespace)); err != nil {
		if k8serrors.IsForbidden(err) {
			slog.Warn("namespace LimitRange is not readable; Kubernetes admission remains authoritative", "error", err)
		} else {
			return nil, fmt.Errorf("read namespace LimitRange: %w", err)
		}
	}
	if err := reader.List(ctx, &policy.quotas, client.InNamespace(namespace)); err != nil {
		if k8serrors.IsForbidden(err) {
			slog.Warn("namespace ResourceQuota is not readable; Kubernetes admission remains authoritative", "error", err)
		} else {
			return nil, fmt.Errorf("read namespace ResourceQuota: %w", err)
		}
	}
	return policy, nil
}

// checkAgentResourceAdmission catches namespace limits which are knowable
// before Kubernetes admission. Quota usage and admission webhooks can change
// between this read and pod creation, so the scheduler remains authoritative.
func checkAgentResourceAdmission(ctx context.Context, reader client.Reader, namespace string, cpu, memory resource.Quantity, current *kyberv1.Agent) error {
	policy, err := loadAgentAdmissionPolicy(ctx, reader, namespace)
	if err != nil {
		return err
	}
	return policy.check(cpu, memory, current)
}

func (policy *agentAdmissionPolicy) check(cpu, memory resource.Quantity, current *kyberv1.Agent) error {
	podCPU, podMemory := cpu.DeepCopy(), memory.DeepCopy()
	podCPU.Add(agentPodOverheadCPU)
	podMemory.Add(agentPodOverheadMemory)
	podLimitCPU, podLimitMemory := cpu.DeepCopy(), memory.DeepCopy()
	podLimitCPU.Add(agentPodLimitOverheadCPU)
	podLimitMemory.Add(agentPodLimitOverheadMemory)
	for i := range policy.limits.Items {
		for _, item := range policy.limits.Items[i].Spec.Limits {
			checkCPU, checkMemory := cpu, memory
			if item.Type == corev1.LimitTypePod {
				checkCPU, checkMemory = podLimitCPU, podLimitMemory
			} else if item.Type != corev1.LimitTypeContainer {
				continue
			}
			if err := checkResourceBounds("cpu", checkCPU, item.Min, item.Max); err != nil {
				return fmt.Errorf("LimitRange %s: %w", policy.limits.Items[i].Name, err)
			}
			if err := checkResourceBounds("memory", checkMemory, item.Min, item.Max); err != nil {
				return fmt.Errorf("LimitRange %s: %w", policy.limits.Items[i].Name, err)
			}
		}
	}
	for i := range policy.quotas.Items {
		for _, bound := range []struct {
			name  corev1.ResourceName
			value resource.Quantity
		}{
			{corev1.ResourceRequestsCPU, podCPU},
			{corev1.ResourceRequestsMemory, podMemory},
			{corev1.ResourceLimitsCPU, podLimitCPU},
			{corev1.ResourceLimitsMemory, podLimitMemory},
		} {
			hard, found := policy.quotas.Items[i].Spec.Hard[bound.name]
			if !found {
				continue
			}
			usage := resource.Quantity{}
			if reported, ok := policy.quotas.Items[i].Status.Used[bound.name]; ok {
				usage = reported.DeepCopy()
			}
			delta := bound.value.DeepCopy()
			if current != nil && (current.Status.Phase == kyberv1.AgentPhaseRunning || current.Status.Phase == kyberv1.AgentPhaseStarting) {
				oldBudget := resource.Quantity{}
				if bound.name == corev1.ResourceRequestsCPU || bound.name == corev1.ResourceLimitsCPU {
					oldBudget = current.Spec.Resources.CPU.DeepCopy()
				} else {
					oldBudget = current.Spec.Resources.Memory.DeepCopy()
				}
				if bound.name == corev1.ResourceRequestsCPU {
					oldBudget.Add(agentPodOverheadCPU)
				} else if bound.name == corev1.ResourceRequestsMemory {
					oldBudget.Add(agentPodOverheadMemory)
				} else if bound.name == corev1.ResourceLimitsCPU {
					oldBudget.Add(agentPodLimitOverheadCPU)
				} else {
					oldBudget.Add(agentPodLimitOverheadMemory)
				}
				if usage.Cmp(oldBudget) >= 0 {
					delta.Sub(oldBudget)
				}
			}
			usage.Add(delta)
			if bound.value.Cmp(hard) > 0 || usage.Cmp(hard) > 0 {
				return fmt.Errorf("ResourceQuota %s: %s would exceed the namespace limit", policy.quotas.Items[i].Name, bound.name)
			}
		}
	}
	return nil
}

func checkResourceBounds(name string, value resource.Quantity, min, max corev1.ResourceList) error {
	key := corev1.ResourceMemory
	if name == "cpu" {
		key = corev1.ResourceCPU
	}
	if bound, found := min[key]; found && value.Cmp(bound) < 0 {
		return fmt.Errorf("%s is below the minimum %s", name, bound.String())
	}
	if bound, found := max[key]; found && value.Cmp(bound) > 0 {
		return fmt.Errorf("%s exceeds the maximum %s", name, bound.String())
	}
	return nil
}
