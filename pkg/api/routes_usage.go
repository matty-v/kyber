package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/usagestore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type usageCursor struct {
	Kind         string    `json:"k"`
	Snapshot     string    `json:"s"`
	AfterTime    time.Time `json:"t,omitempty"`
	AfterID      string    `json:"i,omitempty"`
	AfterOrdinal int64     `json:"o,omitempty"`
}

type usageQuery struct {
	Filter usagestore.ReadFilter
	Limit  int
	Cursor usageCursor
	Hash   [32]byte
	Owner  string
}

func (s *Server) parseUsageQuery(w http.ResponseWriter, r *http.Request, kind string) (usageQuery, bool) {
	var q usageQuery
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return q, false
	}
	if !s.requireScope(w, r, "", "usage-read", ScopeUsageRead) {
		return q, false
	}
	if s.UsageStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "usage_unavailable", "durable usage storage unavailable")
		return q, false
	}
	caller := callerFrom(r.Context())
	if caller == nil || caller.CredentialID == "" || caller.CredentialGeneration == 0 {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "stable credential identity required")
		return q, false
	}
	params := r.URL.Query()
	allowed := map[string]bool{"agent": true, "start": true, "end": true, "runtime": true, "model": true, "route": true, "upstreamProvider": true, "limit": true, "cursor": true}
	if kind == "aggregates" {
		allowed["bucket"] = true
	}
	for key, values := range params {
		if !allowed[key] || len(values) != 1 {
			writeJSONError(w, http.StatusBadRequest, "invalid_query", "invalid usage query parameter")
			return q, false
		}
	}
	if kind == "aggregates" && params.Get("bucket") != "day" {
		writeJSONError(w, http.StatusBadRequest, "invalid_bucket", "bucket must be day")
		return q, false
	}
	startRaw, endRaw := params.Get("start"), params.Get("end")
	if startRaw == "" || endRaw == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_range", "start and end are required")
		return q, false
	}
	var err error
	if kind == "aggregates" {
		q.Filter.Start, err = time.Parse("2006-01-02", startRaw)
		if err == nil {
			q.Filter.End, err = time.Parse("2006-01-02", endRaw)
		}
	} else {
		q.Filter.Start, err = time.Parse(time.RFC3339, startRaw)
		if err == nil {
			q.Filter.End, err = time.Parse(time.RFC3339, endRaw)
		}
	}
	if err != nil || !q.Filter.Start.Before(q.Filter.End) || q.Filter.End.After(time.Now().UTC().Add(24*time.Hour)) ||
		(kind == "events" && q.Filter.End.Sub(q.Filter.Start) > 90*24*time.Hour) ||
		(kind == "aggregates" && q.Filter.End.Sub(q.Filter.Start) > 400*24*time.Hour) {
		writeJSONError(w, http.StatusBadRequest, "invalid_range", "usage time range is invalid")
		return q, false
	}
	q.Filter.Runtime = params.Get("runtime")
	q.Filter.Model = params.Get("model")
	q.Filter.RouteID = params.Get("route")
	q.Filter.UpstreamProvider = params.Get("upstreamProvider")
	if q.Filter.UpstreamProvider == "unknown" {
		q.Filter.UpstreamProvider = ""
		q.Filter.ProviderUnknown = true
	}
	if len(q.Filter.Runtime) > 32 || len(q.Filter.Model) > 256 || len(q.Filter.RouteID) > 128 || len(q.Filter.UpstreamProvider) > 128 {
		writeJSONError(w, http.StatusBadRequest, "invalid_filter", "usage filter is too long")
		return q, false
	}
	q.Limit = 100
	if raw := params.Get("limit"); raw != "" {
		q.Limit, err = strconv.Atoi(raw)
		if err != nil || q.Limit < 1 || q.Limit > 500 {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 500")
			return q, false
		}
	}
	if raw := params.Get("cursor"); raw != "" {
		if len(raw) > 1024 {
			writeJSONError(w, http.StatusBadRequest, "invalid_cursor", "invalid usage cursor")
			return q, false
		}
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(raw)
		if decodeErr != nil || json.Unmarshal(decoded, &q.Cursor) != nil || q.Cursor.Kind != kind || q.Cursor.Snapshot == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_cursor", "invalid usage cursor")
			return q, false
		}
		if _, err := uuid.Parse(q.Cursor.Snapshot); err != nil ||
			(kind == "events" && (q.Cursor.AfterTime.IsZero() || uuid.Validate(q.Cursor.AfterID) != nil)) ||
			(kind == "aggregates" && q.Cursor.AfterOrdinal < 1) {
			writeJSONError(w, http.StatusBadRequest, "invalid_cursor", "invalid usage cursor")
			return q, false
		}
	}
	agentName := params.Get("agent")
	if agentName != "" && len(utilvalidation.IsDNS1123Subdomain(agentName)) != 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_agent", "invalid agent name")
		return q, false
	}
	reader := s.APIReader
	if reader == nil {
		reader = s.K8sClient
	}
	if reader == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent_unavailable", "agent registry unavailable")
		return q, false
	}
	if agentName != "" {
		if !caller.AgentResources.Has(s.Namespace + "/" + agentName) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
			return q, false
		}
		agent := &kyberv1.Agent{}
		if err := reader.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: agentName}, agent); err != nil {
			if apierrors.IsNotFound(err) {
				writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
			} else {
				writeJSONError(w, http.StatusServiceUnavailable, "agent_unavailable", "agent registry unavailable")
			}
			return q, false
		}
		q.Filter.AgentUIDs = []string{string(agent.UID)}
	} else {
		list := &kyberv1.AgentList{}
		if err := reader.List(r.Context(), list, client.InNamespace(s.Namespace)); err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "agent_unavailable", "agent registry unavailable")
			return q, false
		}
		for _, agent := range list.Items {
			if caller.AgentResources.Has(s.Namespace + "/" + agent.Name) {
				q.Filter.AgentUIDs = append(q.Filter.AgentUIDs, string(agent.UID))
			}
		}
	}
	if len(q.Filter.AgentUIDs) > 1000 {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "too_many_agents", "narrow the usage query to one agent")
		return q, false
	}
	sort.Strings(q.Filter.AgentUIDs)
	identity := struct {
		Kind   string
		Filter usagestore.ReadFilter
	}{kind, q.Filter}
	encoded, _ := json.Marshal(identity)
	q.Hash = sha256.Sum256(encoded)
	q.Owner = fmt.Sprintf("%s:%d:%s", caller.CredentialID, caller.CredentialGeneration, caller.PrincipalID)
	return q, true
}

func encodeUsageCursor(c usageCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	q, ok := s.parseUsageQuery(w, r, "events")
	if !ok {
		return
	}
	if len(q.Filter.AgentUIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"events": []usagestore.Event{}, "nextCursor": nil, "coverage": map[string]any{"detectedGapCount": 0, "guaranteedComplete": false}})
		return
	}
	var eventSequence, gapSequence int64
	var err error
	if q.Cursor.Snapshot == "" {
		q.Cursor.Snapshot, eventSequence, gapSequence, err = s.UsageStore.CreateEventSnapshot(r.Context(), q.Owner, q.Hash)
	} else {
		eventSequence, gapSequence, err = s.UsageStore.EventSnapshot(r.Context(), q.Cursor.Snapshot, q.Owner, q.Hash)
	}
	if err != nil {
		writeUsageReadError(w, err)
		return
	}
	page, err := s.UsageStore.ListEvents(r.Context(), q.Filter, eventSequence, gapSequence, q.Cursor.AfterTime, q.Cursor.AfterID, q.Limit)
	if err != nil {
		writeUsageReadError(w, err)
		return
	}
	var next any
	if page.HasMore {
		next = encodeUsageCursor(usageCursor{Kind: "events", Snapshot: q.Cursor.Snapshot, AfterTime: page.NextIngestedAt, AfterID: page.NextID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": page.Events, "nextCursor": next, "coverage": map[string]any{"detectedGapCount": page.GapCount, "guaranteedComplete": false}})
}

func (s *Server) handleUsageAggregates(w http.ResponseWriter, r *http.Request) {
	q, ok := s.parseUsageQuery(w, r, "aggregates")
	if !ok {
		return
	}
	if len(q.Filter.AgentUIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"bucket": "day", "rows": []usagestore.DailyAggregate{}, "nextCursor": nil, "coverage": map[string]any{"detectedGapCount": 0, "guaranteedComplete": false}})
		return
	}
	var err error
	if q.Cursor.Snapshot == "" {
		q.Cursor.Snapshot, err = s.UsageStore.CreateDailySnapshot(r.Context(), q.Owner, q.Hash, q.Filter)
	}
	if err != nil {
		writeUsageReadError(w, err)
		return
	}
	page, err := s.UsageStore.ListDailySnapshot(r.Context(), q.Cursor.Snapshot, q.Owner, q.Hash, q.Cursor.AfterOrdinal, q.Limit)
	if err != nil {
		writeUsageReadError(w, err)
		return
	}
	var next any
	if page.HasMore {
		next = encodeUsageCursor(usageCursor{Kind: "aggregates", Snapshot: q.Cursor.Snapshot, AfterOrdinal: page.NextOrdinal})
	}
	writeJSON(w, http.StatusOK, map[string]any{"bucket": "day", "rows": page.Rows, "nextCursor": next, "coverage": map[string]any{"detectedGapCount": page.GapCount, "guaranteedComplete": false}})
}

func writeUsageReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, usagestore.ErrReadSnapshotExpired):
		writeJSONError(w, http.StatusGone, "cursor_expired", "usage cursor expired or access changed")
	case errors.Is(err, usagestore.ErrInvalid):
		writeJSONError(w, http.StatusBadRequest, "invalid_query", "invalid usage query")
	case errors.Is(err, usagestore.ErrReadSnapshotTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "query_too_large", "narrow the usage time range or agent")
	default:
		writeJSONError(w, http.StatusServiceUnavailable, "usage_unavailable", "usage storage unavailable")
	}
}
