package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/inbound"
	"github.com/matty-v/kyber/pkg/requeststore"
)

// A JSON string can encode one decoded byte as a six-byte \uXXXX escape. Keep
// the wire bounded while allowing worst-case escaping of fields that satisfy
// the store's decoded-byte hard limits; the fixed allowance covers keys,
// delimiters, and reasonable insignificant whitespace.
const maxAgentRequestBodyBytes = 6*(requeststore.HardMaxPromptBytes+requeststore.HardMaxCorrelationBytes) + 1024

var agentRequestIDPattern = regexp.MustCompile(`^req_[a-f0-9]{32}$`)

type submitAgentRequest struct {
	Prompt      string `json:"prompt"`
	Correlation string `json:"correlation,omitempty"`
}

type agentRequestResponse struct {
	ID        string              `json:"id"`
	Status    requeststore.Status `json:"status"`
	CreatedAt string              `json:"createdAt"`
	ExpiresAt string              `json:"expiresAt"`
	UpdatedAt string              `json:"updatedAt,omitempty"`
	Response  string              `json:"response,omitempty"`
	Error     string              `json:"error,omitempty"`
}

// handleAgentRequests dispatches the request subtree:
//
//	POST /api/v1/agents/{agent}/requests
//	GET  /api/v1/agents/{agent}/requests/{requestID}
func (s *Server) handleAgentRequests(w http.ResponseWriter, r *http.Request, agentName, subpath string) {
	subpath = strings.Trim(subpath, "/")
	if subpath == "" {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if !s.requireScope(w, r, agentName, "request-submit", ScopeRequestsWrite) {
			return
		}
		s.submitAgentRequest(w, r, agentName)
		return
	}
	if strings.Contains(subpath, "/") || !agentRequestIDPattern.MatchString(subpath) {
		writeJSONError(w, http.StatusNotFound, "not_found", "request not found")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !s.requireScope(w, r, agentName, "request-read", ScopeRequestsRead) {
		return
	}
	s.getAgentRequest(w, r, agentName, subpath)
}

func (s *Server) submitAgentRequest(w http.ResponseWriter, r *http.Request, agentName string) {
	if s.RequestStore == nil || s.InboundQueue == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "request_service_unavailable", "agent requests are unavailable")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return
		}
		slog.Error("requests: failed to get agent", "agent", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}
	if !agent.Spec.RequestReplyEnabled {
		writeJSONError(w, http.StatusForbidden, "agent_requests_disabled", "bounded requests are disabled for this agent")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAgentRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input submitAgentRequest
	if err := decoder.Decode(&input); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds the configured limit")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "request body must contain one JSON object")
		return
	}

	var request *requeststore.Request
	for attempts := 0; attempts < 3; attempts++ {
		id, err := newAgentRequestID()
		if err != nil {
			slog.Error("requests: failed to generate request ID", "agent", agentName, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to create request")
			return
		}
		request, err = s.RequestStore.Create(r.Context(), agentName, id, input.Prompt, input.Correlation)
		if errors.Is(err, requeststore.ErrConflict) {
			continue
		}
		if err != nil {
			writeRequestStoreError(w, err)
			return
		}
		break
	}
	if request == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "request_store_error", "failed to create request")
		return
	}

	job := inbound.Job{
		Agent:         agentName,
		Binding:       "platform-request",
		RequestID:     request.ID,
		Envelope:      requestEnvelope(request),
		EnqueuedAt:    time.Now().UTC(),
		Kind:          inbound.JobKindRequest,
		DeliverBefore: request.ExpiresAt,
		OnDelivery: func(ctx context.Context, outcome inbound.DeliveryOutcome) {
			s.recordRequestDelivery(ctx, agentName, request.ID, outcome)
		},
	}
	if err := s.InboundQueue.Enqueue(job); err != nil {
		// The request was created atomically before enqueue. Make it terminal so
		// it does not consume an outstanding slot after a rejected submission.
		if failErr := s.RequestStore.Fail(r.Context(), agentName, request.ID, requeststore.FailureDelivery); failErr != nil {
			slog.Warn("requests: failed to close rejected request", "agent", agentName, "request_id", request.ID, "error", failErr)
		}
		if errors.Is(err, inbound.ErrQueueFull) {
			recordAgentRequestTerminal(r.Context(), "enqueue", "queue_full", agentName, request.ID, request.CreatedAt)
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "queue_full", "agent request queue is full")
			return
		}
		recordAgentRequestTerminal(r.Context(), "enqueue", "delivery_failed", agentName, request.ID, request.CreatedAt)
		writeJSONError(w, http.StatusServiceUnavailable, "request_dispatch_unavailable", "failed to enqueue request")
		return
	}
	recordAgentRequestEvent(r.Context(), "submit", "accepted", agentName, request.ID)

	w.Header().Set("Location", "/api/v1/agents/"+agentName+"/requests/"+request.ID)
	writeJSON(w, http.StatusAccepted, requestResponse(request, false))
}

func requestEnvelope(request *requeststore.Request) string {
	prompt, _ := json.Marshal(request.Prompt)
	return fmt.Sprintf("[kyber-request:%s] agent=%s\nrequest:\n  id: %s\n  expires_at: %s\n  prompt_json: %s\naction:\n  Handle only this request. Finish it by calling kyber-request-reply.respond with this request id and your bounded response.\n",
		request.ID, request.Agent, request.ID, request.ExpiresAt.UTC().Format(time.RFC3339Nano), prompt)
}

func (s *Server) recordRequestDelivery(ctx context.Context, agent, requestID string, outcome inbound.DeliveryOutcome) {
	request, _ := s.RequestStore.Get(ctx, agent, requestID)
	var err error
	switch outcome {
	case inbound.DeliveryDispatched:
		err = s.RequestStore.MarkDispatched(ctx, agent, requestID)
	case inbound.DeliveryAgentUnavailable:
		err = s.RequestStore.Fail(ctx, agent, requestID, requeststore.FailureAgentUnavailable)
	case inbound.DeliveryFailed:
		err = s.RequestStore.Fail(ctx, agent, requestID, requeststore.FailureDelivery)
	default:
		err = fmt.Errorf("unknown delivery outcome %q", outcome)
	}
	if err != nil && !errors.Is(err, requeststore.ErrNotFound) && !errors.Is(err, requeststore.ErrConflict) {
		slog.Error("requests: failed to record delivery outcome", "agent", agent, "request_id", requestID, "outcome", outcome, "error", err)
	}
	if err == nil {
		createdAt := time.Time{}
		if request != nil {
			createdAt = request.CreatedAt
		}
		if outcome == inbound.DeliveryDispatched {
			recordAgentRequestEvent(ctx, "delivery", string(outcome), agent, requestID)
		} else {
			recordAgentRequestTerminal(ctx, "delivery", string(outcome), agent, requestID, createdAt)
		}
	}
}

func (s *Server) getAgentRequest(w http.ResponseWriter, r *http.Request, agentName, requestID string) {
	if s.RequestStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "request_service_unavailable", "agent requests are unavailable")
		return
	}
	request, err := s.RequestStore.Get(r.Context(), agentName, requestID)
	if err != nil {
		if errors.Is(err, requeststore.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", "request not found")
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "request_store_error", "failed to read request")
		return
	}
	writeJSON(w, http.StatusOK, requestResponse(request, true))
}

func requestResponse(request *requeststore.Request, includeResult bool) agentRequestResponse {
	response := agentRequestResponse{
		ID:        request.ID,
		Status:    request.Status,
		CreatedAt: request.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: request.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	if !request.UpdatedAt.IsZero() {
		response.UpdatedAt = request.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if includeResult && request.Status == requeststore.StatusCompleted {
		response.Response = request.Response
	}
	if includeResult && request.Status == requeststore.StatusFailed {
		response.Error = string(request.FailureCode)
	}
	return response
}

func newAgentRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "req_" + hex.EncodeToString(random[:]), nil
}

func writeRequestStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, requeststore.ErrInvalidRequest):
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
	case errors.Is(err, requeststore.ErrPromptTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "prompt_too_large", "prompt exceeds the configured limit")
	case errors.Is(err, requeststore.ErrCorrelationTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "correlation_too_large", "correlation exceeds the configured limit")
	case errors.Is(err, requeststore.ErrOutstandingLimit):
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusTooManyRequests, "too_many_requests", "agent has too many outstanding requests")
	default:
		writeJSONError(w, http.StatusServiceUnavailable, "request_store_error", "failed to create request")
	}
}
