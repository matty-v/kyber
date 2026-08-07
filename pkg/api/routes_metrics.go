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
	"github.com/matty-v/kyber/pkg/tokenstore"
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
	case "tokens":
		s.handleMetricsTokens(w, r)
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

// TokenUsageResponse wraps per-agent token counts and estimated cost.
type TokenUsageResponse struct {
	Agent   string             `json:"agent"`
	Model   string             `json:"model"`
	Tokens  map[string]float64 `json:"tokens"`
	CostUSD float64            `json:"costUsd"`
	// Priced is the fail-loud sentinel (kyber#487): false means the model has
	// no entry in provider-rates, so CostUSD is a placeholder 0 the PWA renders
	// as "—"/unpriced rather than a believable $0.0000. Mirrors the
	// contextWindowKnown badge idiom (PR-E #379).
	Priced bool `json:"priced"`
}

// tokenTypes is the set of token-delta components the read path sums per
// (agent, model) pair and the write path records as separate time series.
// Must stay aligned with tokenstore.TokenDelta's fields and the accumulator's
// hash fields (input/cache_creation/cache_read/output).
var tokenTypes = []string{"input", "cache_creation", "cache_read", "output"}

// handleMetricsTokens serves GET /api/v1/metrics/tokens?start=&end=
// Returns per-agent/model token usage and estimated cost, scoped to the window.
//
// Data source priority:
//  1. Redis MetricsStore (Tier 1) — windowed per-(agent,model,type) token time
//     series read with RangeQuery over the absolute start/end, enumerated via
//     the accumulator. Authoritative when configured and any token data exists:
//     returns its result even when the window yields zero rows (so an
//     out-of-range window returns [] rather than resurrecting all-time data —
//     kyber#428).
//  2. Prometheus TSDB (Tier 2) — windowed increase() evaluated at end; used as
//     fallback only when MetricsStore is absent or no token data has ever existed.
func (s *Server) handleMetricsTokens(w http.ResponseWriter, r *http.Request) {
	start, end, ok := metricsTimeRangeParams(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "start and end (unix seconds, 1m–7d range) required")
		return
	}

	rateTable := metrics.LoadRateTable(s.MetricsConfig.TokenRatesPath)

	// Tier 1: windowed Redis token time series (authoritative when token data exists).
	if s.MetricsStore != nil && s.TokenAccumulator != nil {
		if out, ok := s.tokenUsageWindowedFromMetricsStore(r, rateTable, start, end); ok {
			writeJSON(w, http.StatusOK, out)
			return
		}
	}

	// Tier 2: Prometheus TSDB fallback — evaluate increase() at end so a
	// past/future window returns data for that window, not anchored to now.
	tsdb := metrics.NewTSDBClient(s.MetricsConfig)
	window := fmt.Sprintf("%ds", end-start)
	samples, _ := tsdb.QueryInstantAt(
		r.Context(),
		fmt.Sprintf(metricsTokensQuery, s.Namespace, window),
		end,
	)

	type agentModelKey struct{ agent, model string }
	grouped := map[agentModelKey]map[string]float64{}
	for _, sample := range samples {
		agent := sample.Labels["agent"]
		model := sample.Labels["model"]
		tokenType := sample.Labels["token_type"]
		if agent == "" || model == "" || tokenType == "" {
			continue
		}
		key := agentModelKey{agent, model}
		if grouped[key] == nil {
			grouped[key] = map[string]float64{}
		}
		grouped[key][tokenType] += sample.Value
	}

	out := make([]TokenUsageResponse, 0, len(grouped))
	for k, counts := range grouped {
		cost, priced := rateTable.ComputeCostKnown(k.model, counts)
		out = append(out, TokenUsageResponse{
			Agent:   k.agent,
			Model:   k.model,
			Tokens:  counts,
			CostUSD: cost,
			Priced:  priced,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// tokenUsageWindowedFromMetricsStore returns per-(agent,model) token usage and
// cost scoped to [start, end] by summing the per-type Redis token time series.
// The accumulator enumerates which (agent, model) pairs exist; for each pair we
// RangeQuery the per-type series (one per tokenTypes entry) and sum the points
// in the window — a near-mirror of activityFromMetricsStore.
//
// Unlike activityFromMetricsStore (which returns (nil,false) on empty so the
// caller falls back to Prometheus), this path is AUTHORITATIVE once the
// accumulator shows token data has ever existed: it returns its result even
// when the window yields zero rows. Otherwise an out-of-range window would fall
// through to Tier-2 and resurrect the all-time/recent set — the exact #428 bug.
// It returns (nil, false) only when no token data exists at all (empty
// accumulator), so a never-populated cluster can still fall back to Prometheus.
func (s *Server) tokenUsageWindowedFromMetricsStore(r *http.Request, rateTable metrics.RateTable, start, end int64) ([]TokenUsageResponse, bool) {
	pairs, err := s.TokenAccumulator.GetAll(r.Context(), s.Namespace)
	if err != nil || len(pairs) == 0 {
		// No token data has ever existed — let the caller try Tier-2.
		return nil, false
	}

	out := make([]TokenUsageResponse, 0, len(pairs))
	for _, p := range pairs {
		counts := map[string]float64{}
		hasPoints := false
		for _, tt := range tokenTypes {
			key := metricsstore.TokenUsageKey(s.Namespace, p.Agent, p.Model, tt)
			pts, err := s.MetricsStore.RangeQuery(r.Context(), key, start, end)
			if err != nil {
				continue
			}
			var sum float64
			for _, pt := range pts {
				sum += pt.Value
			}
			counts[tt] = sum
			if len(pts) > 0 {
				hasPoints = true
			}
		}
		// Skip pairs with no points anywhere in the window so an out-of-range
		// window yields [] rather than a list of zero rows.
		if !hasPoints {
			continue
		}
		cost, priced := rateTable.ComputeCostKnown(p.Model, counts)
		out = append(out, TokenUsageResponse{
			Agent:   p.Agent,
			Model:   p.Model,
			Tokens:  counts,
			CostUSD: cost,
			Priced:  priced,
		})
	}
	// Authoritative even when out is empty (do NOT fall through to Tier-2).
	return out, true
}

// tokenUsageFromAccumulator reads the Redis accumulator and converts entries to
// a TokenUsageResponse slice of all-time totals. Since kyber#428 the windowed
// Tier-1 path (tokenUsageWindowedFromMetricsStore) backs the panel; the
// accumulator is retained as the (agent, model) enumerator and is reserved for
// an explicit all-time view. Returns (nil, false) when the accumulator returns
// no data.
func (s *Server) tokenUsageFromAccumulator(r *http.Request, rateTable metrics.RateTable) ([]TokenUsageResponse, bool) {
	counts, err := s.TokenAccumulator.GetAll(r.Context(), s.Namespace)
	if err != nil || len(counts) == 0 {
		return nil, false
	}
	out := make([]TokenUsageResponse, 0, len(counts))
	for _, c := range counts {
		tokenMap := tokenCountsToMap(c)
		cost, priced := rateTable.ComputeCostKnown(c.Model, tokenMap)
		out = append(out, TokenUsageResponse{
			Agent:   c.Agent,
			Model:   c.Model,
			Tokens:  tokenMap,
			CostUSD: cost,
			Priced:  priced,
		})
	}
	return out, true
}

// tokenCountsToMap converts AgentModelCounts to the map[string]float64 shape
// expected by TokenUsageResponse.Tokens.
func tokenCountsToMap(c tokenstore.AgentModelCounts) map[string]float64 {
	return map[string]float64{
		"input":          float64(c.Input),
		"cache_creation": float64(c.CacheCreation),
		"cache_read":     float64(c.CacheRead),
		"output":         float64(c.Output),
	}
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
	metricsTokensQuery        = `increase(kyber_agent_tokens_total{namespace="%s"}[%s])`
	metricsNodeCPUQuery       = `100 - (avg by (node) (rate(node_cpu_seconds_total{mode="idle",namespace="%s"}[2m])) * 100)`
	metricsNodeMemUsedQuery   = `node_memory_MemTotal_bytes{namespace="%s"} - node_memory_MemAvailable_bytes{namespace="%s"}`
	metricsNodeMemTotalQuery  = `node_memory_MemTotal_bytes{namespace="%s"}`
	metricsNodeDiskUsedQuery  = `node_filesystem_size_bytes{namespace="%s",mountpoint="/"} - node_filesystem_free_bytes{namespace="%s",mountpoint="/"}`
	metricsNodeDiskTotalQuery = `node_filesystem_size_bytes{namespace="%s",mountpoint="/"}`
	metricsStateChangesQuery  = `increase(kyber_agent_state_changes_total{namespace="%s"}[%s])`
)
