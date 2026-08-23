package api

import (
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const managedPodSelector = "app.kubernetes.io/part-of=kyber"
const maxGenericLogTail = 10_000

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
	Namespace        string                     `json:"namespace"`
	Pod              string                     `json:"pod"`
	PodUID           string                     `json:"podUid"`
	Component        string                     `json:"component"`
	Workload         string                     `json:"workload"`
	Agent            string                     `json:"agent,omitempty"`
	Machine          string                     `json:"machine,omitempty"`
	Phase            corev1.PodPhase            `json:"phase"`
	Sources          []string                   `json:"sources"`
	LiveAvailable    bool                       `json:"liveAvailable"`
	ArchiveAvailable bool                       `json:"archiveAvailable"`
	Containers       []loggingContainerResponse `json:"containers"`
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

// handleLoggingLogs streams one validated container from a currently managed
// pod. Pod UID is required so a selection cannot silently move to a recreated
// pod with the same name between discovery and the read.
func (s *Server) handleLoggingLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if s.K8sClient == nil || s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "log streaming not available")
		return
	}
	q := r.URL.Query()
	if source := q.Get("source"); source != "" && source != "kubelet" {
		writeJSONError(w, http.StatusBadRequest, "invalid_source", "source must be 'kubelet'")
		return
	}
	podName, podUID, container := q.Get("pod"), q.Get("podUid"), q.Get("container")
	if podName == "" || podUID == "" || container == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_target", "pod, podUid, and container are required")
		return
	}
	if value := q.Get("tail"); value != "" {
		tail, err := strconv.ParseInt(value, 10, 64)
		if err != nil || tail < 0 || tail > maxGenericLogTail {
			writeJSONError(w, http.StatusBadRequest, "invalid_tail", "tail must be between 0 and 10000")
			return
		}
	}
	if value := q.Get("since"); value != "" {
		if duration, err := time.ParseDuration(value); err != nil || duration <= 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_since", "since must be a positive duration")
			return
		}
	}

	pod := &corev1.Pod{}
	if err := s.K8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: podName}, pod); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "logging target not found")
			return
		}
		slog.Error("failed to resolve generic logging target", "pod", podName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to resolve logging target")
		return
	}
	if pod.Labels["app.kubernetes.io/part-of"] != "kyber" || string(pod.UID) != podUID || !loggingPodHasContainer(pod, container) {
		writeJSONError(w, http.StatusNotFound, "not_found", "logging target not found")
		return
	}

	release, ok := s.tryAcquireReadSlot()
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeJSONError(w, http.StatusTooManyRequests, "too_many_concurrent_reads", "too many concurrent log reads in flight; retry shortly")
		return
	}
	defer release()

	opts := parsePodLogOptions(r)
	opts.Container = container
	stream, err := s.Clientset.CoreV1().Pods(s.Namespace).GetLogs(podName, opts).Stream(r.Context())
	if err != nil {
		slog.Error("failed to open generic log stream", "pod", podName, "container", container, "error", err)
		writeJSONError(w, http.StatusBadGateway, "stream_error", "failed to open log stream")
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)
	if _, err := w.Write([]byte("\n")); err != nil {
		return
	}
	if canFlush {
		flusher.Flush()
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.Debug("generic log stream ended", "pod", podName, "container", container, "error", err)
			}
			return
		}
	}
}

func loggingPodHasContainer(pod *corev1.Pod, name string) bool {
	for _, container := range pod.Spec.InitContainers {
		if container.Name == name {
			return true
		}
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return true
		}
	}
	return false
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
		Phase: pod.Status.Phase, Sources: []string{"kubelet"}, LiveAvailable: s.Clientset != nil,
		ArchiveAvailable: false, Containers: containers,
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
