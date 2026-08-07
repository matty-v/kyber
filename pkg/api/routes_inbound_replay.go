package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/inbound"
)

// inboundEventReplayed is the Kubernetes Event reason emitted when an
// operator replays an inbound run. PascalCase to match the receiver-side
// Event reasons in routes_inbound.go.
const inboundEventReplayed = "InboundReplayed"

// replayInboundRun handles
// POST /api/v1/agents/{name}/inbound-bindings/{binding}/replay/{requestId}.
//
// Pulls the rendered envelope from the EnvelopeCache (written by the
// receiver after a matched Decide), generates a fresh request ID, and
// re-enqueues a Job carrying the original envelope. Bypasses HMAC
// verification (the original signature is on the original body, not the
// envelope), bypasses dedup (replay is by-definition a duplicate), and
// bypasses filters (the envelope was rendered from filters that already
// passed at receive time).
//
// Failure paths:
//
//	410 Gone           — envelope aged out of the cache (>7 days, or
//	                     never written because s.InboundEnvelopeCache was
//	                     nil at receive time).
//	503 Service Unavailable — EnvelopeCache backend error or no cache /
//	                     queue configured at all.
//	404 Not Found      — agent or binding doesn't exist.
//	429 Too Many       — per-agent queue is at capacity.
//	202 Accepted       — replay queued. Body shape:
//	                     {originalRequestId, newRequestId, status:"queued"}.
func (s *Server) replayInboundRun(w http.ResponseWriter, r *http.Request, agentName, bindingName, originalRequestID string) {
	if s.InboundEnvelopeCache == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"envelope cache not configured on this control plane; replay is unavailable")
		return
	}
	if s.InboundQueue == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"inbound dispatch queue not configured on this control plane")
		return
	}

	// Resolve the agent. This is the management API (already API-key
	// authenticated upstream), so a clean 404 is appropriate — no
	// information-leak posture like the receiver's blanket 401.
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found",
				"agent '"+agentName+"' not found")
			return
		}
		slog.Error("inbound-replay: get agent",
			"agent", agentName, "binding", bindingName,
			"original_request_id", originalRequestID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	// Verify the named binding exists. We don't reuse its filter / HMAC
	// config — the envelope is already rendered — but we DO require the
	// binding to still be on the spec so a deleted binding can't be
	// replayed against. (If you really want to replay a deleted binding,
	// recreate it first.)
	bindingFound := false
	for _, b := range agent.Spec.InboundBindings {
		if b.Name == bindingName {
			bindingFound = true
			break
		}
	}
	if !bindingFound {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"binding '"+bindingName+"' not found on agent '"+agentName+"'")
		return
	}

	// Pull the envelope from the cache.
	cacheCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	envelope, err := s.InboundEnvelopeCache.Get(cacheCtx, originalRequestID)
	cancel()
	if err != nil {
		slog.Error("inbound-replay: envelope cache get failed",
			"agent", agentName, "binding", bindingName,
			"original_request_id", originalRequestID, "error", err)
		writeJSONError(w, http.StatusServiceUnavailable, "cache_unavailable",
			"envelope cache backend is unavailable; try again later")
		return
	}
	if envelope == "" {
		// Custom flat shape per spec: {error, message} at the top level
		// rather than the nested {error:{code,message}} writeJSONError uses.
		// The PWA needs to differentiate "expired" from generic 4xx, so a
		// distinct envelope makes the contract explicit.
		writeJSON(w, http.StatusGone, map[string]string{
			"error":   "envelope_expired",
			"message": "the run's envelope has aged out of the 7-day cache and cannot be replayed",
		})
		return
	}

	// Generate a fresh request ID for the replay so audit + observability
	// can tell apart "we re-fired this run" from "the upstream sent the
	// same body twice".
	newRequestID := uuid.New().String()

	job := inbound.Job{
		Agent:      agentName,
		Binding:    bindingName,
		RequestID:  newRequestID,
		Envelope:   envelope,
		EnqueuedAt: time.Now().UTC(),
	}
	if err := s.InboundQueue.Enqueue(job); err != nil {
		if errors.Is(err, inbound.ErrQueueFull) {
			slog.Warn("inbound-replay: queue full",
				"agent", agentName, "binding", bindingName,
				"original_request_id", originalRequestID,
				"new_request_id", newRequestID)
			writeJSONError(w, http.StatusTooManyRequests, "queue_full",
				"agent inbound queue is full; retry later")
			return
		}
		slog.Error("inbound-replay: enqueue failed",
			"agent", agentName, "binding", bindingName,
			"original_request_id", originalRequestID,
			"new_request_id", newRequestID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error",
			"failed to enqueue replay job")
		return
	}

	// Audit the replay as a regular dispatched run, but stamp the Error
	// field with the original request ID so an operator can tell from
	// the ring buffer that this was a replay and which run it came from.
	now := metav1.Now()
	startedAt := now
	s.recordInboundRun(r.Context(), agentName, bindingName, newRequestID, &startedAt,
		inboundOutcomeDispatched, "",
		"replay of "+originalRequestID)

	s.emitInboundEvent(agent, corev1.EventTypeNormal, inboundEventReplayed,
		"binding=%s replayed (original_request_id=%s, new_request_id=%s)",
		bindingName, originalRequestID, newRequestID)

	slog.Info("inbound-replay: queued",
		"agent", agentName, "binding", bindingName,
		"original_request_id", originalRequestID,
		"new_request_id", newRequestID)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"originalRequestId": originalRequestID,
		"newRequestId":      newRequestID,
		"status":            "queued",
	})
}
