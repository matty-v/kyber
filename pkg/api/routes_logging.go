package api

import (
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const managedPodSelector = "app.kubernetes.io/part-of=kyber"

var managedLoggingComponents = map[string]struct{}{
	"control-plane": {}, "node-agent": {}, "status-sidecar": {},
	"discord-sidecar": {}, "telegram-sidecar": {}, "self-upgrade": {},
	"transcript-compact": {},
}

type loggingSettingsResponse struct {
	GlobalLevel          string            `json:"globalLevel"`
	ComponentOverrides   map[string]string `json:"componentOverrides"`
	ArchiveRetentionDays int               `json:"archiveRetentionDays"`
	ManagedBy            string            `json:"managedBy"`
}

type loggingTargetResponse struct {
	Namespace  string                     `json:"namespace"`
	Pod        string                     `json:"pod"`
	PodUID     string                     `json:"podUid"`
	Component  string                     `json:"component"`
	Workload   string                     `json:"workload"`
	Agent      string                     `json:"agent,omitempty"`
	Machine    string                     `json:"machine,omitempty"`
	Phase      corev1.PodPhase            `json:"phase"`
	Containers []loggingContainerResponse `json:"containers"`
}

type loggingContainerResponse struct {
	Name           string `json:"name"`
	Component      string `json:"component"`
	EffectiveLevel string `json:"effectiveLevel"`
	ManagedLevel   bool   `json:"managedLevel"`
	Init           bool   `json:"init"`
}

func (s *Server) handleLoggingSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	overrides := make(map[string]string, len(s.LoggingComponentLevels))
	for component, level := range s.LoggingComponentLevels {
		overrides[component] = level
	}
	global := s.LoggingGlobalLevel
	if global == "" {
		global = "info"
	}
	writeJSON(w, http.StatusOK, loggingSettingsResponse{
		GlobalLevel: global, ComponentOverrides: overrides,
		ArchiveRetentionDays: s.LoggingArchiveRetention, ManagedBy: "helm",
	})
}

func (s *Server) handleLoggingTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if s.K8sClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "Kubernetes client not configured")
		return
	}

	pods := &corev1.PodList{}
	if err := s.K8sClient.List(r.Context(), pods,
		client.InNamespace(s.Namespace), client.MatchingLabels{"app.kubernetes.io/part-of": "kyber"}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list logging targets")
		return
	}

	targets := make([]loggingTargetResponse, 0, len(pods.Items))
	for i := range pods.Items {
		targets = append(targets, s.loggingTarget(&pods.Items[i]))
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Component != targets[j].Component {
			return targets[i].Component < targets[j].Component
		}
		return targets[i].Pod < targets[j].Pod
	})
	writeJSON(w, http.StatusOK, map[string]any{"targets": targets, "selector": managedPodSelector})
}

func (s *Server) loggingTarget(pod *corev1.Pod) loggingTargetResponse {
	component := pod.Labels["app.kubernetes.io/component"]
	workload := loggingWorkload(pod)
	containers := make([]loggingContainerResponse, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, container := range pod.Spec.InitContainers {
		containers = append(containers, s.loggingContainer(component, container.Name, true))
	}
	for _, container := range pod.Spec.Containers {
		containers = append(containers, s.loggingContainer(component, container.Name, false))
	}
	return loggingTargetResponse{
		Namespace: pod.Namespace, Pod: pod.Name, PodUID: string(pod.UID), Component: component,
		Workload: workload, Agent: pod.Labels["kyber.io/agent"], Machine: pod.Labels["kyber.io/machine"],
		Phase: pod.Status.Phase, Containers: containers,
	}
}

func (s *Server) loggingContainer(podComponent, name string, init bool) loggingContainerResponse {
	component := podComponent
	switch name {
	case "kyber-status-sidecar":
		component = "status-sidecar"
	case "kyber-mcp-discord":
		component = "discord-sidecar"
	case "kyber-mcp-telegram":
		component = "telegram-sidecar"
	case "upgrade":
		component = "self-upgrade"
	}
	_, managed := managedLoggingComponents[component]
	level := "unmanaged"
	if managed {
		level = s.LoggingGlobalLevel
		if level == "" {
			level = "info"
		}
		if override := s.LoggingComponentLevels[component]; override != "" {
			level = override
		}
	}
	return loggingContainerResponse{Name: name, Component: component, EffectiveLevel: level, ManagedLevel: managed, Init: init}
}

func loggingWorkload(pod *corev1.Pod) string {
	if value := pod.Labels["kyber.io/agent"]; value != "" {
		return value
	}
	if value := pod.Labels["kyber.io/machine"]; value != "" {
		return value
	}
	owner := metav1.GetControllerOf(pod)
	if owner != nil {
		return owner.Kind + "/" + owner.Name
	}
	return pod.Name
}
