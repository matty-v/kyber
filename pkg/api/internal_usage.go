package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/usagestore"
)

const maxUsageRequestBytes = 8 << 10

func decodeUsageRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUsageRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return usagestore.ErrInvalid
	}
	return nil
}

func (s *InternalServer) usageAgent(w http.ResponseWriter, r *http.Request, name, claimedUID, fingerprint string) (*kyberv1.Agent, usagestore.RouteRevision, bool) {
	if s.usageStore == nil || s.k8sClient == nil {
		http.Error(w, "durable usage unavailable", http.StatusServiceUnavailable)
		return nil, usagestore.RouteRevision{}, false
	}
	reader := s.usageAgentReader
	if reader == nil {
		reader = s.k8sClient
	}
	agent := &kyberv1.Agent{}
	if err := reader.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, agent); err != nil {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
		return nil, usagestore.RouteRevision{}, false
	}
	// The claim is a freshness guard for an old pod after an Agent name is
	// reused. It is never used as the persisted identity.
	if claimedUID == "" || string(agent.UID) == "" || claimedUID != string(agent.UID) {
		http.Error(w, "agent lifetime changed", http.StatusConflict)
		return nil, usagestore.RouteRevision{}, false
	}
	endpointURL := ""
	if agent.Spec.Inference != nil {
		endpointURL = agent.Spec.Inference.BaseURL
	}
	route, err := usagestore.RouteRevisionFor(agent.Spec.Runtime, string(agent.Spec.Secrets.AuthType), endpointURL)
	if err != nil {
		http.Error(w, "invalid agent route", http.StatusServiceUnavailable)
		return nil, usagestore.RouteRevision{}, false
	}
	want, err := route.FingerprintHex()
	if err != nil || fingerprint == "" || fingerprint != want {
		http.Error(w, "agent route changed", http.StatusConflict)
		return nil, usagestore.RouteRevision{}, false
	}
	return agent, route, true
}

func sourceFormat(runtime string) string {
	switch runtime {
	case "claude":
		return "claude-jsonl"
	case "codex":
		return "codex-rollout"
	case "hermes":
		return "hermes-call-log"
	default:
		return ""
	}
}

func writeUsageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, usagestore.ErrInvalid):
		http.Error(w, "invalid usage evidence", http.StatusBadRequest)
	case errors.Is(err, usagestore.ErrConflict):
		http.Error(w, "usage identity conflict", http.StatusConflict)
	case errors.Is(err, usagestore.ErrOutsideRetention):
		http.Error(w, "usage outside retention", http.StatusUnprocessableEntity)
	default:
		http.Error(w, "durable usage unavailable", http.StatusServiceUnavailable)
	}
}

func (s *InternalServer) handleUsageEventPost(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body tokenreport.UsageEventRequest
	if err := decodeUsageRequest(w, r, &body); err != nil {
		http.Error(w, "invalid usage body", http.StatusBadRequest)
		return
	}
	agent, route, ok := s.usageAgent(w, r, name, body.AgentUID, body.RouteFingerprint)
	if !ok {
		return
	}
	evidence := body.Evidence
	if evidence.Runtime != route.Runtime {
		http.Error(w, "runtime mismatch", http.StatusBadRequest)
		return
	}
	uid := string(agent.UID)
	routeID, err := s.usageStore.ResolveRoute(r.Context(), uid, route)
	if err != nil {
		writeUsageError(w, err)
		return
	}
	var providerLabel *string
	if evidence.ReportedProviderLabel != "" {
		providerLabel = &evidence.ReportedProviderLabel
	}
	event := usagestore.Event{
		AgentUID: uid, Agent: agent.Name, ObservedAt: evidence.ObservedAt,
		Runtime: evidence.Runtime, SourceFormat: sourceFormat(evidence.Runtime), SourceVersion: "1",
		SourceIdentity: evidence.SourceIdentity, Coverage: "observed", ConfiguredRoute: route.Kind, RouteID: routeID,
		Model: evidence.Model, ModelEvidence: evidence.ModelEvidence, ReportedProviderLabel: providerLabel,
		InputUncached: evidence.InputUncached, Output: evidence.Output, CacheRead: evidence.CacheRead,
		CacheWrite: evidence.CacheWrite, RawUsage: evidence.RawUsage,
	}
	cursor := usagestore.SourceCursor{Namespace: body.Cursor.Namespace, Offset: body.Cursor.Offset}
	if _, _, err := s.usageStore.IngestWithCursor(r.Context(), event, &cursor); err != nil {
		writeUsageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *InternalServer) handleUsageGapPost(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body tokenreport.UsageGapRequest
	if err := decodeUsageRequest(w, r, &body); err != nil {
		http.Error(w, "invalid usage body", http.StatusBadRequest)
		return
	}
	agent, route, ok := s.usageAgent(w, r, name, body.AgentUID, body.RouteFingerprint)
	if !ok {
		return
	}
	if body.Runtime != route.Runtime {
		http.Error(w, "runtime mismatch", http.StatusBadRequest)
		return
	}
	gap := usagestore.Gap{AgentUID: string(agent.UID), Runtime: body.Runtime, SourceFormat: sourceFormat(body.Runtime),
		SourceIdentity: body.SourceIdentity, DetectedAt: body.DetectedAt, Reason: body.Reason}
	cursor := usagestore.SourceCursor{Namespace: body.Cursor.Namespace, Offset: body.Cursor.Offset}
	if err := s.usageStore.RecordGapWithCursor(r.Context(), gap, &cursor); err != nil {
		writeUsageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
