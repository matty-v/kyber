package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const managedPodSelector = "app.kubernetes.io/part-of=kyber"
const maxGenericLogTail = 10_000
const maxArchivedLoggingTargets = 1000

// Export is deliberately larger than the measured 8 MiB interactive-view cap
// (see archive_reader.go's production line-size measurements) but remains
// bounded: 64 MiB is eight view windows and one export runs at a time. A
// 31-day ceiling matches the default 30-day object lifecycle with one day of
// clock/expiry margin.
const maxLoggingExportBytes = 64 << 20
const maxLoggingExportWindow = 31 * 24 * time.Hour

var errLoggingExportLimit = errors.New("logging export byte limit reached")

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
	live := map[GenericArchiveSelection]struct{}{}
	for i := range pods.Items {
		target := s.loggingTarget(&pods.Items[i])
		targets = append(targets, target)
		for _, container := range target.Containers {
			live[GenericArchiveSelection{Component: target.Component, Workload: target.Workload, PodUID: target.PodUID, Container: container.Name}] = struct{}{}
		}
	}
	if s.PlatformArchiveReader != nil {
		selections, err := s.PlatformArchiveReader.ListContainerSelections(r.Context(), maxArchivedLoggingTargets)
		if err != nil {
			slog.Error("failed to list archived logging targets", "error", err)
		} else {
			targets = append(targets, s.archivedLoggingTargets(selections, live)...)
		}
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
	if s.K8sClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "log streaming not available")
		return
	}
	q := r.URL.Query()
	source := q.Get("source")
	if source == "" {
		source = "kubelet"
	}
	if source != "kubelet" && source != "archive" {
		writeJSONError(w, http.StatusBadRequest, "invalid_source", "source must be 'kubelet' or 'archive'")
		return
	}
	podName, podUID, container := q.Get("pod"), q.Get("podUid"), q.Get("container")
	if podName == "" || podUID == "" || container == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_target", "pod, podUid, and container are required")
		return
	}
	if value := q.Get("tail"); source == "kubelet" && value != "" {
		tail, err := strconv.ParseInt(value, 10, 64)
		if err != nil || tail < 0 || tail > maxGenericLogTail {
			writeJSONError(w, http.StatusBadRequest, "invalid_tail", "tail must be between 0 and 10000")
			return
		}
	}
	if value := q.Get("since"); source == "kubelet" && value != "" {
		if duration, err := time.ParseDuration(value); err != nil || duration <= 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_since", "since must be a positive duration")
			return
		}
	}

	if source == "archive" {
		selection, ok := archiveSelectionFromQuery(q)
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "invalid_target", "component, workload, podUid, and container are required")
			return
		}
		if !s.isArchivedSelection(r, selection) {
			writeJSONError(w, http.StatusNotFound, "not_found", "logging target not found")
			return
		}
		s.serveGenericArchivedLogs(w, r, selection)
		return
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
	if s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "live log streaming not available")
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

func (s *Server) handleLoggingExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if s.K8sClient == nil || s.PlatformArchiveReader == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "archive export not available")
		return
	}
	q := r.URL.Query()
	podName, podUID, container := q.Get("pod"), q.Get("podUid"), q.Get("container")
	if podName == "" || podUID == "" || container == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_target", "pod, podUid, and container are required")
		return
	}
	format := q.Get("format")
	if format == "" {
		format = "ndjson"
	}
	if format != "ndjson" && format != "text" {
		writeJSONError(w, http.StatusBadRequest, "invalid_format", "format must be 'ndjson' or 'text'")
		return
	}
	since, until, errMsg := parseArchiveWindow(r)
	if errMsg != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_window", errMsg)
		return
	}
	if until.Sub(since) > maxLoggingExportWindow {
		writeJSONError(w, http.StatusBadRequest, "invalid_window", "export window must not exceed 31 days")
		return
	}
	selection, ok := archiveSelectionFromQuery(q)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_target", "component, workload, podUid, and container are required")
		return
	}
	if !s.isArchivedSelection(r, selection) {
		writeJSONError(w, http.StatusNotFound, "not_found", "logging target not found")
		return
	}
	release, ok := s.tryAcquireExportSlot()
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeJSONError(w, http.StatusTooManyRequests, "too_many_concurrent_exports", "another log export is in flight; retry shortly")
		return
	}
	defer release()

	extension, contentType := "ndjson", "application/x-ndjson"
	if format == "text" {
		extension, contentType = "log", "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="kyber-`+podName+`-`+container+`.`+extension+`"`)
	w.Header().Set("Trailer", "X-Kyber-Log-Truncated")
	w.WriteHeader(http.StatusOK)

	written := int64(0)
	maxBytes := s.MaxExportBytes
	if maxBytes <= 0 {
		maxBytes = maxLoggingExportBytes
	}
	err := s.PlatformArchiveReader.StreamContainerRecords(r.Context(), selection, since, until, func(raw string, line LogLine) error {
		output := raw + "\n"
		if format == "text" {
			output = line.Text + "\n"
		}
		if written+int64(len(output)) > maxBytes {
			return errLoggingExportLimit
		}
		n, err := io.WriteString(w, output)
		written += int64(n)
		return err
	})
	if err == nil {
		return
	}
	w.Header().Set("X-Kyber-Log-Truncated", "true")
	if format == "ndjson" {
		_, _ = io.WriteString(w, `{"kyber_export":{"truncated":true,"reason":"limit_or_upstream_error"}}`+"\n")
	} else {
		_, _ = io.WriteString(w, "[kyber export truncated: limit or upstream error]\n")
	}
	if !errors.Is(err, errLoggingExportLimit) {
		slog.Error("generic log export failed", "pod", podName, "container", container, "error", err)
	}
}

func (s *Server) serveGenericArchivedLogs(w http.ResponseWriter, r *http.Request, selection GenericArchiveSelection) {
	since, until, errMsg := parseArchiveWindow(r)
	if errMsg != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_window", errMsg)
		return
	}
	if s.PlatformArchiveReader == nil {
		message := "archive log surface not configured"
		if s.PlatformArchiveDisabledReason != "" {
			message += ": " + s.PlatformArchiveDisabledReason
		}
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", message)
		return
	}
	release, ok := s.tryAcquireReadSlot()
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeJSONError(w, http.StatusTooManyRequests, "too_many_concurrent_reads", "too many concurrent log reads in flight; retry shortly")
		return
	}
	defer release()
	result, err := s.PlatformArchiveReader.ReadContainerLines(r.Context(), selection, since, until)
	if err != nil {
		slog.Error("generic archive read failed", "pod_uid", selection.PodUID, "container", selection.Container, "error", err)
		writeJSONError(w, http.StatusBadGateway, "archive_read_error", "failed to read archived logs")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if result.Truncated {
		w.Header().Set("X-Kyber-Log-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	for _, line := range result.Lines {
		if _, err := io.WriteString(w, line.Text+"\n"); err != nil {
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
	sources := []string{"kubelet"}
	if s.PlatformArchiveReader != nil {
		sources = append(sources, "archive")
	}
	return loggingTargetResponse{
		Namespace: pod.Namespace, Pod: pod.Name, PodUID: string(pod.UID), Component: component,
		Workload: workload, Agent: pod.Labels["kyber.io/agent"], Machine: pod.Labels["kyber.io/machine"],
		Phase: pod.Status.Phase, Sources: sources, LiveAvailable: s.Clientset != nil,
		ArchiveAvailable: s.PlatformArchiveReader != nil, Containers: containers,
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
	return pod.Labels["app.kubernetes.io/component"]
}

func archiveSelectionFromQuery(q mapQuery) (GenericArchiveSelection, bool) {
	selection := GenericArchiveSelection{Component: q.Get("component"), Workload: q.Get("workload"), PodUID: q.Get("podUid"), Container: q.Get("container")}
	for _, value := range []string{selection.Component, selection.Workload, selection.PodUID, selection.Container} {
		if !validArchiveSegment(value) {
			return GenericArchiveSelection{}, false
		}
	}
	return selection, true
}

type mapQuery interface{ Get(string) string }

func (s *Server) isArchivedSelection(r *http.Request, want GenericArchiveSelection) bool {
	selections, err := s.PlatformArchiveReader.ListContainerSelections(r.Context(), maxArchivedLoggingTargets)
	if err != nil {
		slog.Error("failed to validate archived logging target", "error", err)
		return false
	}
	for _, selection := range selections {
		if selection == want {
			return true
		}
	}
	return false
}

func (s *Server) archivedLoggingTargets(selections []GenericArchiveSelection, live map[GenericArchiveSelection]struct{}) []loggingTargetResponse {
	byPod := map[string]*loggingTargetResponse{}
	for _, selection := range selections {
		if _, ok := live[selection]; ok {
			continue
		}
		key := selection.Component + "\x00" + selection.Workload + "\x00" + selection.PodUID
		target := byPod[key]
		if target == nil {
			target = &loggingTargetResponse{Namespace: s.Namespace, Pod: "archived-" + selection.PodUID, PodUID: selection.PodUID, Component: selection.Component, Workload: selection.Workload, Phase: "Archived", Sources: []string{"archive"}, ArchiveAvailable: true}
			switch selection.Component {
			case "agent":
				target.Agent = selection.Workload
			case "node-agent":
				target.Machine = selection.Workload
			}
			byPod[key] = target
		}
		target.Containers = append(target.Containers, s.loggingContainer(selection.Component, selection.Container, false))
	}
	result := make([]loggingTargetResponse, 0, len(byPod))
	for _, target := range byPod {
		result = append(result, *target)
	}
	return result
}
