package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/metrics"
	"github.com/matty-v/kyber/pkg/metricsstore"
)

// metricsTimeRangeParams validates and extracts start/end query parameters as Unix timestamps.
// Rejects ranges shorter than 1 minute or longer than 7 days.
func metricsTimeRangeParams(r *http.Request) (start, end int64, ok bool) {
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	if startStr == "" || endStr == "" {
		return 0, 0, false
	}
	s, err1 := strconv.ParseInt(startStr, 10, 64)
	e, err2 := strconv.ParseInt(endStr, 10, 64)
	if err1 != nil || err2 != nil || s >= e {
		return 0, 0, false
	}
	dur := e - s
	const minSecs = 60
	const maxSecs = 7 * 24 * 3600
	if dur < minSecs || dur > maxSecs {
		return 0, 0, false
	}
	return s, e, true
}

// rangeStep picks a Prometheus step string appropriate for the requested window.
func rangeStep(start, end int64) string {
	dur := end - start
	switch {
	case dur <= 3600:
		return "60s"
	case dur <= 6*3600:
		return "5m"
	case dur <= 24*3600:
		return "15m"
	default:
		return "1h"
	}
}

// handleMetrics dispatches /api/v1/metrics/* sub-paths.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	const prefix = "/api/v1/metrics/"
	sub := r.URL.Path[len(prefix):]

	switch sub {
	case "summary":
		s.handleMetricsSummary(w, r)
	case "activity":
		s.handleMetricsActivity(w, r)
	case "working-time":
		s.handleMetricsWorkingTime(w, r)
	case "last-active":
		s.handleMetricsLastActive(w, r)
	case "nodes":
		s.handleMetricsNodes(w, r)
	case "state-changes":
		s.handleMetricsStateChanges(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "not_found", "not found")
	}
}

// handleMetricsSummary serves GET /api/v1/metrics/summary — fleet counts from CRDs.
func (s *Server) handleMetricsSummary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "max-age=15")
	summary, err := metrics.GetFleetSummary(r.Context(), s.K8sClient, s.Namespace)
	if err != nil {
		slog.Error("metrics/summary: failed to list agents", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read agent CRDs")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// handleMetricsLastActive serves GET /api/v1/metrics/last-active — per-agent heartbeat table.
func (s *Server) handleMetricsLastActive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "max-age=15")
	entries, err := metrics.GetLastActive(r.Context(), s.K8sClient, s.Namespace, s.MetricsConfig)
	if err != nil {
		slog.Error("metrics/last-active: failed to list agents", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read agent CRDs")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleMetricsActivity serves GET /api/v1/metrics/activity?start=&end=
// Returns per-agent activity state rate series.
//
// Data source priority:
//  1. Redis MetricsStore (Tier 1) — when configured and populated.
//  2. Prometheus TSDB (Tier 2) — fallback when MetricsStore is absent or empty.
func (s *Server) handleMetricsActivity(w http.ResponseWriter, r *http.Request) {
	start, end, ok := metricsTimeRangeParams(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "start and end (unix seconds, 1m–7d range) required")
		return
	}

	// Tier 1: Redis MetricsStore.
	if s.MetricsStore != nil {
		if series, ok := s.activityFromMetricsStore(r, start, end); ok {
			writeJSON(w, http.StatusOK, series)
			return
		}
	}

	// Tier 2: Prometheus TSDB fallback.
	tsdb := metrics.NewTSDBClient(s.MetricsConfig)
	window := fmt.Sprintf("%ds", end-start)
	series, _ := tsdb.QueryRange(
		r.Context(),
		fmt.Sprintf(metricsActivityQuery, s.Namespace, window),
		start, end, rangeStep(start, end),
	)
	writeJSON(w, http.StatusOK, seriesOrEmpty(series))
}

// activityFromMetricsStore reads per-agent activity time-series from MetricsStore.
// It enumerates agents from the K8s CRD list (same source the summary panel uses)
// and for each agent queries all three known activity states. Returns (nil, false)
// when no data is found so the caller can fall back to Prometheus.
func (s *Server) activityFromMetricsStore(r *http.Request, start, end int64) ([]metrics.Series, bool) {
	agentList := &kyberv1.AgentList{}
	if err := s.K8sClient.List(r.Context(), agentList, client.InNamespace(s.Namespace)); err != nil {
		return nil, false
	}

	var series []metrics.Series
	for _, agent := range agentList.Items {
		for _, state := range activityStates {
			key := metricsstore.ActivityKey(s.Namespace, agent.Name, state)
			pts, err := s.MetricsStore.RangeQuery(r.Context(), key, start, end)
			if err != nil || len(pts) == 0 {
				continue
			}
			spoints := make([]metrics.SeriesPoint, len(pts))
			for i, p := range pts {
				spoints[i] = metrics.SeriesPoint{Timestamp: p.Timestamp, Value: p.Value}
			}
			series = append(series, metrics.Series{
				Labels: map[string]string{
					"namespace": s.Namespace,
					"agent":     agent.Name,
					"state":     state,
				},
				Points: spoints,
			})
		}
	}
	if len(series) == 0 {
		return nil, false
	}
	return series, true
}

// activityStates is the set of agent activity states the read path
// enumerates when querying MetricsStore. Must stay aligned with the CP
// write path's whitelist (pkg/statechangestore.ValidState); a state the
// write path accepts but the read path doesn't enumerate would silently
// land in Redis and never reach the PWA — kyber#368 Cause G. The runtime
// emits 'unknown' on activity-detector errors
// (pkg/tokenreport/activity.go's ActivityUnknown), so it must round-trip
// end to end.
// TODO(state-vocab): consolidate this list and pkg/statechangestore's
// ValidState into a shared pkg/agentstate package so the next addition
// only touches one source of truth.
var activityStates = []string{"working", "idle", "paused", "unknown"}

// handleMetricsWorkingTime serves GET /api/v1/metrics/working-time?start=&end=
// Returns cumulative working hours per agent.
//
// Data source priority:
//  1. Redis MetricsStore (Tier 1) — when configured and populated.
//  2. Prometheus TSDB (Tier 2) — fallback when MetricsStore is absent or empty.
func (s *Server) handleMetricsWorkingTime(w http.ResponseWriter, r *http.Request) {
	start, end, ok := metricsTimeRangeParams(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "start and end (unix seconds, 1m–7d range) required")
		return
	}

	// Tier 1: Redis MetricsStore — sum the "working" state series per agent.
	if s.MetricsStore != nil {
		if series, ok := s.workingTimeFromMetricsStore(r, start, end); ok {
			writeJSON(w, http.StatusOK, series)
			return
		}
	}

	// Tier 2: Prometheus TSDB fallback.
	tsdb := metrics.NewTSDBClient(s.MetricsConfig)
	window := fmt.Sprintf("%ds", end-start)
	series, _ := tsdb.QueryRange(
		r.Context(),
		fmt.Sprintf(metricsWorkingTimeQuery, s.Namespace, window),
		start, end, rangeStep(start, end),
	)
	writeJSON(w, http.StatusOK, seriesOrEmpty(series))
}

// workingTimeFromMetricsStore returns per-agent working-time series by reading
// the "working" state series and expressing values in hours (seconds / 3600).
func (s *Server) workingTimeFromMetricsStore(r *http.Request, start, end int64) ([]metrics.Series, bool) {
	agentList := &kyberv1.AgentList{}
	if err := s.K8sClient.List(r.Context(), agentList, client.InNamespace(s.Namespace)); err != nil {
		return nil, false
	}

	var series []metrics.Series
	for _, agent := range agentList.Items {
		key := metricsstore.ActivityKey(s.Namespace, agent.Name, "working")
		pts, err := s.MetricsStore.RangeQuery(r.Context(), key, start, end)
		if err != nil || len(pts) == 0 {
			continue
		}
		spoints := make([]metrics.SeriesPoint, len(pts))
		for i, p := range pts {
			spoints[i] = metrics.SeriesPoint{Timestamp: p.Timestamp, Value: p.Value / 3600}
		}
		series = append(series, metrics.Series{
			Labels: map[string]string{
				"namespace": s.Namespace,
				"agent":     agent.Name,
			},
			Points: spoints,
		})
	}
	if len(series) == 0 {
		return nil, false
	}
	return series, true
}

// NodeResourcesResponse holds live resource gauges for one node.
type NodeResourcesResponse struct {
	Node           string  `json:"node"`
	CPUPercent     float64 `json:"cpuPercent"`
	MemUsedBytes   float64 `json:"memUsedBytes"`
	MemTotalBytes  float64 `json:"memTotalBytes"`
	DiskUsedBytes  float64 `json:"diskUsedBytes"`
	DiskTotalBytes float64 `json:"diskTotalBytes"`
}

// handleMetricsNodes serves GET /api/v1/metrics/nodes
// Returns live node resource gauges (instant queries, no time range).
//
// Data source priority:
//  1. NodeStore (Tier 1) — latest samples from the node-agent reporter.
//  2. Prometheus TSDB (Tier 2) — fallback.
func (s *Server) handleMetricsNodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "max-age=20")

	// Tier 1: NodeStore.
	if s.NodeStore != nil {
		samples, err := s.NodeStore.GetAllNodes(r.Context(), s.Namespace)
		if err == nil && len(samples) > 0 {
			out := make([]NodeResourcesResponse, 0, len(samples))
			for _, sample := range samples {
				out = append(out, NodeResourcesResponse{
					Node:           sample.Node,
					CPUPercent:     sample.CPUPercent,
					MemUsedBytes:   sample.MemUsedBytes,
					MemTotalBytes:  sample.MemTotalBytes,
					DiskUsedBytes:  sample.DiskUsedBytes,
					DiskTotalBytes: sample.DiskTotalBytes,
				})
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
	}

	// Tier 2: Prometheus TSDB fallback.
	tsdb := metrics.NewTSDBClient(s.MetricsConfig)

	ctx := r.Context()
	cpuSamples, _ := tsdb.QueryInstant(ctx, fmt.Sprintf(metricsNodeCPUQuery, s.Namespace))
	memUsedSamples, _ := tsdb.QueryInstant(ctx, fmt.Sprintf(metricsNodeMemUsedQuery, s.Namespace, s.Namespace))
	memTotalSamples, _ := tsdb.QueryInstant(ctx, fmt.Sprintf(metricsNodeMemTotalQuery, s.Namespace))
	diskUsedSamples, _ := tsdb.QueryInstant(ctx, fmt.Sprintf(metricsNodeDiskUsedQuery, s.Namespace, s.Namespace))
	diskTotalSamples, _ := tsdb.QueryInstant(ctx, fmt.Sprintf(metricsNodeDiskTotalQuery, s.Namespace))

	byNode := map[string]*NodeResourcesResponse{}
	ensure := func(node string) *NodeResourcesResponse {
		if byNode[node] == nil {
			byNode[node] = &NodeResourcesResponse{Node: node}
		}
		return byNode[node]
	}

	for _, smpl := range cpuSamples {
		ensure(smpl.Labels["node"]).CPUPercent = smpl.Value
	}
	for _, smpl := range memUsedSamples {
		ensure(smpl.Labels["node"]).MemUsedBytes = smpl.Value
	}
	for _, smpl := range memTotalSamples {
		ensure(smpl.Labels["node"]).MemTotalBytes = smpl.Value
	}
	for _, smpl := range diskUsedSamples {
		ensure(smpl.Labels["node"]).DiskUsedBytes = smpl.Value
	}
	for _, smpl := range diskTotalSamples {
		ensure(smpl.Labels["node"]).DiskTotalBytes = smpl.Value
	}

	out := make([]NodeResourcesResponse, 0, len(byNode))
	for _, v := range byNode {
		out = append(out, *v)
	}
	writeJSON(w, http.StatusOK, out)
}

// StateChangeEntry is the response shape for /api/v1/metrics/state-changes.
type StateChangeEntry struct {
	Agent   string  `json:"agent"`
	ToState string  `json:"toState"`
	Count   float64 `json:"count"`
}

// handleMetricsStateChanges serves GET /api/v1/metrics/state-changes?start=&end=
// Returns per-agent state-change counts.
//
// Data source priority:
//  1. StateChangeAccumulator (Tier 1) — always-available all-time totals.
//  2. Prometheus TSDB (Tier 2) — windowed data via PromQL.
func (s *Server) handleMetricsStateChanges(w http.ResponseWriter, r *http.Request) {
	start, end, ok := metricsTimeRangeParams(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "start and end (unix seconds, 1m–7d range) required")
		return
	}

	// Tier 1: StateChangeAccumulator.
	if s.StateChangeAccumulator != nil {
		counts, err := s.StateChangeAccumulator.GetAll(r.Context(), s.Namespace)
		if err == nil && len(counts) > 0 {
			out := make([]StateChangeEntry, 0, len(counts))
			for _, c := range counts {
				out = append(out, StateChangeEntry{
					Agent:   c.Agent,
					ToState: c.ToState,
					Count:   float64(c.Count),
				})
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
	}

	// Tier 2: Prometheus TSDB fallback.
	tsdb := metrics.NewTSDBClient(s.MetricsConfig)
	window := fmt.Sprintf("%ds", end-start)
	samples, _ := tsdb.QueryInstant(
		r.Context(),
		fmt.Sprintf(metricsStateChangesQuery, s.Namespace, window),
	)
	out := make([]StateChangeEntry, 0, len(samples))
	for _, sample := range samples {
		out = append(out, StateChangeEntry{
			Agent:   sample.Labels["agent"],
			ToState: sample.Labels["to_state"],
			Count:   sample.Value,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// seriesOrEmpty returns series if non-nil, otherwise an empty slice (avoids JSON null).
func seriesOrEmpty(series []metrics.Series) []metrics.Series {
	if series == nil {
		return []metrics.Series{}
	}
	return series
}

// PromQL templates used by the metrics handlers. All label matchers are
// derived from the server's validated Namespace field — no user input reaches
// these query strings.
const (
	metricsActivityQuery      = `rate(kyber_agent_activity_state_seconds_total{namespace="%s"}[%s])`
	metricsWorkingTimeQuery   = `increase(kyber_agent_activity_state_seconds_total{namespace="%s",state="working"}[%s]) / 3600`
	metricsNodeCPUQuery       = `100 - (avg by (node) (rate(node_cpu_seconds_total{mode="idle",namespace="%s"}[2m])) * 100)`
	metricsNodeMemUsedQuery   = `node_memory_MemTotal_bytes{namespace="%s"} - node_memory_MemAvailable_bytes{namespace="%s"}`
	metricsNodeMemTotalQuery  = `node_memory_MemTotal_bytes{namespace="%s"}`
	metricsNodeDiskUsedQuery  = `node_filesystem_size_bytes{namespace="%s",mountpoint="/"} - node_filesystem_free_bytes{namespace="%s",mountpoint="/"}`
	metricsNodeDiskTotalQuery = `node_filesystem_size_bytes{namespace="%s",mountpoint="/"}`
	metricsStateChangesQuery  = `increase(kyber_agent_state_changes_total{namespace="%s"}[%s])`
)
