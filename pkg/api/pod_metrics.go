package api

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

type AgentContainerMetrics struct {
	CPUUsageMillicores int64
	MemoryUsedBytes    int64
}

type AgentMetricsProvider interface {
	AgentContainer(ctx context.Context, namespace, podName string) (AgentContainerMetrics, error)
}

type kubernetesAgentMetrics struct{ client dynamic.Interface }

func NewKubernetesAgentMetrics(cfg *rest.Config) (AgentMetricsProvider, error) {
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &kubernetesAgentMetrics{client: client}, nil
}

func (m *kubernetesAgentMetrics) AgentContainer(ctx context.Context, namespace, podName string) (AgentContainerMetrics, error) {
	gvr := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
	pod, err := m.client.Resource(gvr).Namespace(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return AgentContainerMetrics{}, err
	}
	containers, found, err := unstructured.NestedSlice(pod.Object, "containers")
	if err != nil || !found {
		return AgentContainerMetrics{}, fmt.Errorf("pod metrics containers unavailable")
	}
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok || container["name"] != "agent" {
			continue
		}
		usage, ok := container["usage"].(map[string]any)
		if !ok {
			break
		}
		cpu, err := resource.ParseQuantity(fmt.Sprint(usage["cpu"]))
		if err != nil {
			return AgentContainerMetrics{}, fmt.Errorf("parse agent cpu usage: %w", err)
		}
		memory, err := resource.ParseQuantity(fmt.Sprint(usage["memory"]))
		if err != nil {
			return AgentContainerMetrics{}, fmt.Errorf("parse agent memory usage: %w", err)
		}
		return AgentContainerMetrics{CPUUsageMillicores: cpu.MilliValue(), MemoryUsedBytes: memory.Value()}, nil
	}
	return AgentContainerMetrics{}, fmt.Errorf("agent container missing from pod metrics")
}
