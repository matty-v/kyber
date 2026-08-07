package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/statechangestore"
)

// dns1123Re matches valid Kubernetes DNS-1123 names (letters, digits, hyphens;
// must start and end with alphanumeric). Reused for agent and node name validation.
var dns1123Re = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`)

// validDNS1123 reports whether name is a valid DNS-1123 label.
func validDNS1123(name string) bool {
	return len(name) > 0 && len(name) <= 253 && dns1123Re.MatchString(name)
}

// statusSnapshotRequest is the payload for POST /internal/agents/{name}/status.
// New fields (ActivityStateSeconds, StateTransitions) are optional — payloads
// omitting them are accepted without error (rolling-upgrade contract).
type statusSnapshotRequest struct {
	// ActivityStateSeconds holds per-state cumulative seconds (monotonic).
	// Key is the state name (e.g. "working", "idle", "paused"); value is
	// total seconds spent in that state since pod start.
	ActivityStateSeconds map[string]float64 `json:"activity_state_seconds,omitempty"`

	// StateTransitions lists state changes since the last snapshot.
	StateTransitions []stateTransition `json:"state_transitions,omitempty"`

	// At is the wall-clock time the snapshot was generated (RFC3339).
	// When absent, server-side now is used.
	At string `json:"at,omitempty"`
}

type stateTransition struct {
	From string `json:"from"`
	To   string `json:"to"`
	At   string `json:"at"`
}

// handleStatusSnapshot handles POST /internal/agents/{name}/status.
// The sidecar posts this on a 15-second cadence; the handler:
//  1. Validates agent name (DNS-1123) and input ranges.
//  2. Computes per-state delta seconds vs. the previous snapshot and writes
//     time-series points to MetricsStore.
//  3. Records each state transition in the StateChangeAccumulator.
//
// Returns 200 on success, 400 on validation failure, 503 when MetricsStore
// or StateChangeAccumulator are not configured.
func (s *InternalServer) handleStatusSnapshot(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.metricsStore == nil && s.stateChangeAccum == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	if !validDNS1123(agentName) {
		http.Error(w, "invalid agent name: must conform to DNS-1123", http.StatusBadRequest)
		return
	}

	var req statusSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	// Validate structural malformation hard (negative seconds): these are
	// programmer-bug shapes the sidecar can never legitimately emit, so 400
	// is appropriate. The state-name validation is fail-soft per kyber#360
	// Cause F — a single out-of-vocab state must not silently kill the
	// entire batch (the failure mode that hid the bug for two iterations
	// on the live cluster).
	for state, secs := range req.ActivityStateSeconds {
		if secs < 0 {
			http.Error(w, "negative activity_state_seconds for state "+state, http.StatusBadRequest)
			return
		}
	}

	// Fail-soft state-name validation. Invalid entries are dropped from the
	// batch with a WARN log; the rest of the batch is processed normally.
	// Empty input is fine — the rolling-upgrade contract accepts payloads
	// missing the new fields. See kyber#360 Cause F design proposal.
	for state := range req.ActivityStateSeconds {
		if !statechangestore.ValidState(state) {
			slog.Warn("snapshot: dropping invalid state in activity_state_seconds",
				"state", state, "agent", agentName)
			delete(req.ActivityStateSeconds, state)
		}
	}
	if len(req.StateTransitions) > 0 {
		kept := req.StateTransitions[:0]
		for _, tr := range req.StateTransitions {
			if !statechangestore.ValidState(tr.To) {
				slog.Warn("snapshot: dropping invalid transition",
					"field", "to", "state", tr.To, "agent", agentName)
				continue
			}
			// tr.From is empty for the very first transition (agent starts from no state);
			// only validate when non-empty.
			if tr.From != "" && !statechangestore.ValidState(tr.From) {
				slog.Warn("snapshot: dropping invalid transition",
					"field", "from", "state", tr.From, "agent", agentName)
				continue
			}
			kept = append(kept, tr)
		}
		req.StateTransitions = kept
	}

	now := time.Now().Unix()
	at := now
	if req.At != "" {
		if t, err := time.Parse(time.RFC3339, req.At); err == nil {
			at = t.Unix()
		}
	}

	ctx := r.Context()

	// Compute per-state delta seconds vs the prior snapshot and write as time-series
	// points. The sidecar sends cumulative totals (running sum since pod start);
	// storing deltas lets the PWA sum points over a window to get total seconds in
	// that window — equivalent to Prometheus increase() on the counter series.
	if s.metricsStore != nil && len(req.ActivityStateSeconds) > 0 {
		type stateDelta struct {
			key   string
			delta float64
		}
		var deltas []stateDelta

		s.snapshotMu.Lock()
		if s.snapshotPrior == nil {
			s.snapshotPrior = map[string]map[string]float64{}
		}
		prior := s.snapshotPrior[agentName]
		if prior == nil {
			prior = map[string]float64{}
			s.snapshotPrior[agentName] = prior
		}
		for state, cumulative := range req.ActivityStateSeconds {
			delta := cumulative - prior[state]
			prior[state] = cumulative
			if delta > 0 {
				deltas = append(deltas, stateDelta{
					key:   metricsstore.ActivityKey(s.namespace, agentName, state),
					delta: delta,
				})
			}
		}
		s.snapshotMu.Unlock()

		for _, d := range deltas {
			_ = s.metricsStore.AddPoint(ctx, d.key, at, d.delta)
		}
	}

	// Record state transitions.
	if s.stateChangeAccum != nil {
		for _, tr := range req.StateTransitions {
			_ = s.stateChangeAccum.IncrBy(ctx, s.namespace, agentName, tr.To, 1)
		}
	}

	w.WriteHeader(http.StatusOK)
}
