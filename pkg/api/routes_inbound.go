package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/inbound"
)

// Drop reason strings recorded on Agent.status.inboundRuns[].DropReason.
// Match the dash-separated style declared in agent_types.go's documentation.
const (
	inboundDropSigMismatch    = "sig-mismatch"
	inboundDropMissingSecret  = "missing-secret"
	inboundDropDedup          = "dedup"
	inboundDropUnmatchedEvent = "unmatched-event"
	inboundDropFilterRejected = "filter-rejected"
	inboundDropRateLimited    = "rate-limited"
	inboundDropQueueFull      = "queue-full"

	inboundOutcomeDispatched = "dispatched"
	inboundOutcomeDropped    = "dropped"
)

// Kubernetes Event reason strings emitted on the Agent CR for inbound
// receiver outcomes. PascalCase per k8s convention.
const (
	inboundEventDispatched     = "InboundDispatched"
	inboundEventAuthFailure    = "InboundAuthFailure"
	inboundEventConfigError    = "InboundConfigError"
	inboundEventUnmatched      = "InboundUnmatched"
	inboundEventFilterRejected = "InboundFilterRejected"
	inboundEventRateLimitTrip  = "RateLimitTripped"
	inboundEventQueueFull      = "QueueFull"
)

// InboundRequestIDHeader is the response header carrying the per-request ID
// the platform assigns to each inbound webhook delivery. Mirrored into the
// rendered envelope and into server-side log lines so operators can correlate
// a Telegram-style cURL test with the agent prompt and the controller event.
const InboundRequestIDHeader = "X-Inbound-Request-Id"

// inboundBodyLimit is the maximum body size we accept for an inbound webhook.
// 1 MiB is enough for any reasonable JSON payload from GitHub / Stripe / etc;
// anything bigger is almost certainly an attack or a misconfigured client.
const inboundBodyLimit = 1 << 20

// inboundDedupTTL is how long body-hash dedup entries live. Issue #208 specifies
// a 24-hour window — long enough to absorb GitHub's documented "delivered
// twice" retry behaviour without accidentally suppressing legitimate
// repeat events that happen to share a body (rare, since payloads include
// timestamps).
const inboundDedupTTL = 24 * time.Hour

// webhookSecretKey is the conventional key inside the binding's referenced
// Secret holding the HMAC shared secret. Mirrors the Telegram convention.
const webhookSecretKey = "webhook-secret"

// recordInboundRun is a small wrapper around appendInboundRun that swallows
// the error after logging it. The receiver MUST NOT change its HTTP
// response based on whether the audit append succeeded — a status-write
// failure is observability degradation, not a security issue.
//
// Uses a fresh, short-lived context detached from the request so the
// status patch survives if the caller has already started writing the
// response and the request context is being torn down. (The fake client
// used in tests doesn't care, but production patches go through the real
// API server.)
func (s *Server) recordInboundRun(_ context.Context, agentName, bindingName, requestID string, startedAt *metav1.Time, outcome, dropReason, errStr string) {
	if s == nil || s.K8sClient == nil {
		return
	}
	now := metav1.Now()
	run := kyberv1.AgentInboundRun{
		BindingName: bindingName,
		RequestID:   requestID,
		StartedAt:   startedAt,
		FinishedAt:  &now,
		Outcome:     outcome,
		DropReason:  dropReason,
		Error:       errStr,
	}
	patchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.appendInboundRun(patchCtx, agentName, run); err != nil {
		slog.Warn("inbound: status append failed",
			"agent", agentName, "binding", bindingName,
			"request_id", requestID, "outcome", outcome,
			"drop_reason", dropReason, "error", err)
	}
}

// emitInboundEvent records a per-occurrence Event on the Agent CR. Nil-
// safe: when the recorder is nil (tests, dev) the call is a no-op. The
// caller is responsible for resolving the Agent object first — passing a
// stale or partial object risks an Event with the wrong involvedObject
// reference.
func (s *Server) emitInboundEvent(agent *kyberv1.Agent, eventType, reason, msgFormat string, args ...any) {
	if s == nil || s.Recorder == nil || agent == nil {
		return
	}
	s.Recorder.Eventf(agent, eventType, reason, msgFormat, args...)
}

// unauthorized writes the canonical {"error":"unauthorized"} body with a 401.
// Used for every authentication / binding-resolution failure so an attacker
// can't tell from the response which specific check rejected them. Server
// logs DO carry the precise reason — that's the only place callers reveal
// it.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

// handleInbound serves POST /webhooks/inbound/{agent}/{binding}.
//
// Pipeline (failure modes documented inline next to each step):
//
//  1. Method check — POST only.                         → 405
//  2. Path parse — extract <agent>/<binding>.           → 401 (generic)
//  3. Body read with 1 MiB cap.                         → 413
//  4. Look up Agent CRD.                                → 401 (generic)
//  5. Find binding by name.                             → 401 (generic)
//  6. Load HMAC secret from binding.ExistingSecret.     → 401 (generic)
//  7. inbound.Verify HMAC.                              → 401 (generic)
//  8. Dedup body hash (24h TTL).                        → 200 dedup
//  9. inbound.Decide (matchEvents / filters / fields).  → 200 dropped
//
// 10. Per-binding rate limit.                           → 429
// 11. Enqueue per-agent.                                → 429 queue full
// 12. Return 200 with X-Inbound-Request-Id.
//
// The architectural decision to register at /webhooks/inbound/* (rather than
// /api/v1/inbound/* as the issue suggested) is documented in the PR
// description: /api/v1/* requires API-key auth, but inbound endpoints
// authenticate via per-binding HMAC. Sharing the /webhooks/* auth-bypass
// path with Telegram is the natural fit.
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	// Capture the receiver-side start time so the audit ring buffer can
	// report request latency (FinishedAt - StartedAt). metav1.Now()
	// truncates to second precision, which is fine — these entries are
	// for human triage, not perf benchmarking.
	startedAt := metav1.Now()

	// Reuse the request-ID middleware's UUID when present so the same
	// identifier appears in logs, response header, and the rendered
	// envelope. Generate a fresh UUID if the middleware didn't run (tests
	// using the bare handler) or the header is empty.
	requestID := w.Header().Get(RequestIDHeader)
	if requestID == "" {
		requestID = uuid.New().String()
	}
	w.Header().Set(InboundRequestIDHeader, requestID)

	// Path parse: /webhooks/inbound/{agent}/{binding}
	suffix, ok := trimPrefix(r.URL.Path, "/webhooks/inbound/")
	if !ok {
		slog.Warn("inbound: malformed path prefix", "path", r.URL.Path, "request_id", requestID)
		unauthorized(w)
		return
	}
	suffix = trimLeadingSlash(suffix)
	parts := strings.Split(suffix, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		slog.Warn("inbound: malformed path", "path", r.URL.Path, "request_id", requestID)
		unauthorized(w)
		return
	}
	agentName, bindingName := parts[0], parts[1]

	// Body read with explicit overflow detection. We pass +1 to the limit
	// reader so a body of EXACTLY the limit is accepted while a body of
	// limit+1 is rejected. (Without the +1 we couldn't distinguish "fits
	// exactly" from "overflowed".)
	body, err := io.ReadAll(io.LimitReader(r.Body, inboundBodyLimit+1))
	if err != nil {
		slog.Warn("inbound: read body", "error", err, "request_id", requestID,
			"agent", agentName, "binding", bindingName)
		unauthorized(w)
		return
	}
	if len(body) > inboundBodyLimit {
		slog.Warn("inbound: body too large",
			"size", len(body), "limit", inboundBodyLimit,
			"request_id", requestID, "agent", agentName, "binding", bindingName)
		writeJSONError(w, http.StatusRequestEntityTooLarge, "body_too_large", "body exceeds 1 MiB")
		return
	}

	// Agent CRD lookup.
	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(),
		types.NamespacedName{Name: agentName, Namespace: s.Namespace}, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			slog.Warn("inbound: agent not found",
				"agent", agentName, "binding", bindingName, "request_id", requestID)
		} else {
			slog.Error("inbound: get agent",
				"agent", agentName, "binding", bindingName,
				"error", err, "request_id", requestID)
		}
		unauthorized(w)
		return
	}

	// Find the named binding.
	var binding kyberv1.AgentInboundBinding
	found := false
	for _, b := range agent.Spec.InboundBindings {
		if b.Name == bindingName {
			binding = b
			found = true
			break
		}
	}
	if !found {
		slog.Warn("inbound: binding not found",
			"agent", agentName, "binding", bindingName, "request_id", requestID)
		unauthorized(w)
		return
	}

	// Resolve the HMAC secret from the referenced k8s Secret. A missing or
	// malformed Secret is operator misconfiguration — surface it via an
	// audit entry and a Warning Event so it doesn't get lost in the
	// "everything 401s" noise. We still return the canonical 401 to the
	// caller (no info leak).
	secret := &corev1.Secret{}
	if err := s.K8sClient.Get(r.Context(),
		types.NamespacedName{Name: binding.ExistingSecret, Namespace: s.Namespace}, secret); err != nil {
		if k8serrors.IsNotFound(err) {
			slog.Warn("inbound: hmac secret not found",
				"agent", agentName, "binding", bindingName,
				"secret", binding.ExistingSecret, "request_id", requestID)
		} else {
			slog.Error("inbound: get hmac secret",
				"agent", agentName, "binding", bindingName,
				"secret", binding.ExistingSecret, "error", err, "request_id", requestID)
		}
		s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
			inboundOutcomeDropped, inboundDropMissingSecret,
			fmt.Sprintf("hmac secret %q lookup failed", binding.ExistingSecret))
		s.emitInboundEvent(agent, corev1.EventTypeWarning, inboundEventConfigError,
			"hmac secret %q for binding=%s not found or unreadable (request_id=%s)",
			binding.ExistingSecret, bindingName, requestID)
		unauthorized(w)
		return
	}
	hmacSecret, ok := secret.Data[webhookSecretKey]
	if !ok || len(hmacSecret) == 0 {
		slog.Warn("inbound: hmac secret missing key",
			"agent", agentName, "binding", bindingName,
			"secret", binding.ExistingSecret, "key", webhookSecretKey,
			"request_id", requestID)
		s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
			inboundOutcomeDropped, inboundDropMissingSecret,
			fmt.Sprintf("hmac secret %q missing key %q", binding.ExistingSecret, webhookSecretKey))
		s.emitInboundEvent(agent, corev1.EventTypeWarning, inboundEventConfigError,
			"hmac secret %q for binding=%s is missing key %q (request_id=%s)",
			binding.ExistingSecret, bindingName, webhookSecretKey, requestID)
		unauthorized(w)
		return
	}
	// Optional rotation-grace: when the operator has rotated this binding
	// recently (POST .../rotate-secret), the previous secret remains valid
	// for inboundRotationGrace so an in-flight upstream cutover doesn't
	// drop traffic. The rotate-secret handler stamps webhook-secret-prev
	// + webhook-secret-prev-rotated-at; we only honour the prev key when
	// the timestamp parses AND falls inside the grace window.
	var prevHmacSecret []byte
	if prev, ok := secret.Data[webhookSecretPrevKey]; ok && len(prev) > 0 {
		if rotatedRaw, ok := secret.Data[webhookSecretPrevRotatedAtKey]; ok && len(rotatedRaw) > 0 {
			if rotatedAt, err := time.Parse(time.RFC3339, string(rotatedRaw)); err == nil {
				if time.Since(rotatedAt) < inboundRotationGrace {
					prevHmacSecret = prev
				}
			}
		}
	}

	// Verify HMAC. We split into two outcomes for audit purposes only —
	// the wire response is identical 401 either way:
	//
	//  - Missing signature header: a probe with no header at all is pure
	//    DoS-scanner noise. No status entry, no Event — letting drive-by
	//    scans pollute the audit ring buffer would defeat its purpose.
	//
	//  - Header present but invalid (wrong prefix, bad hex, wrong MAC):
	//    that's an actual auth failure worth surfacing — record an entry
	//    and emit an InboundAuthFailure Event.
	sigHeader := r.Header.Get(binding.SignatureHeader)
	if sigHeader == "" {
		slog.Warn("inbound: signature mismatch",
			"agent", agentName, "binding", bindingName,
			"sig_header", binding.SignatureHeader,
			"has_header", false,
			"request_id", requestID)
		unauthorized(w)
		return
	}
	verifyErr := inbound.Verify(hmacSecret, body, sigHeader, binding.SignaturePrefix)
	matchedSecret := "current"
	if verifyErr != nil && prevHmacSecret != nil {
		// Try the previous secret; rotation grace is in effect.
		if prevErr := inbound.Verify(prevHmacSecret, body, sigHeader, binding.SignaturePrefix); prevErr == nil {
			verifyErr = nil
			matchedSecret = "prev"
		}
	}
	if verifyErr != nil {
		// Lump every Verify failure under a single log key — even on the
		// server side we don't gain anything from differentiating "wrong
		// hex" from "wrong MAC".
		slog.Warn("inbound: signature mismatch",
			"agent", agentName, "binding", bindingName,
			"sig_header", binding.SignatureHeader,
			"has_header", true,
			"request_id", requestID)
		s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
			inboundOutcomeDropped, inboundDropSigMismatch, "")
		s.emitInboundEvent(agent, corev1.EventTypeWarning, inboundEventAuthFailure,
			"signature mismatch on binding=%s (request_id=%s)", bindingName, requestID)
		unauthorized(w)
		return
	}
	// Log which secret matched so an operator can confirm rotation grace
	// is being exercised without blowing the audit budget. INFO level so
	// the line is visible in default log levels but doesn't fire a Warn
	// alert.
	if matchedSecret == "prev" {
		slog.Info("inbound: signature matched previous secret (rotation grace active)",
			"agent", agentName, "binding", bindingName, "request_id", requestID)
	}

	// Dedup. Treat any deduper error as fail-open (log + continue) so a
	// transient Redis blip doesn't black-hole inbound traffic — duplicate
	// dispatches are recoverable, dropped legitimate events are not.
	//
	// Trade-off: SetNX writes the dedup key BEFORE the rate-limit /
	// queue-full / enqueue checks below. So a body that gets rate-limited
	// or rejected by a full queue still occupies the 24h dedup slot — a
	// retry of the same body within that window short-circuits to
	// "deduped" and the agent never sees it. Acceptable for V1 (matches
	// FR-7's "duplicate short-circuits to 200" reading and avoids races
	// where two identical concurrent POSTs both pass dedup); revisit if
	// rate-limit drops become common.
	if s.InboundDeduper != nil {
		dedupCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		seen, derr := s.InboundDeduper.SeenWithin(dedupCtx, body, inboundDedupTTL)
		cancel()
		if derr != nil {
			slog.Warn("inbound: deduper error — failing open",
				"agent", agentName, "binding", bindingName,
				"error", derr, "request_id", requestID)
		} else if seen {
			slog.Info("inbound: deduped",
				"agent", agentName, "binding", bindingName, "request_id", requestID)
			// Per the matrix in routes_inbound docs: dedup is info-only
			// — we record the audit entry so operators can see how often
			// dedup is firing, but we do NOT emit a k8s Event (operators
			// don't need to be alerted; it's a normal protocol behaviour).
			s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
				inboundOutcomeDropped, inboundDropDedup, "")
			writeJSON(w, http.StatusOK, map[string]any{
				"status":     "deduped",
				"request_id": requestID,
			})
			return
		}
	}

	// Decide: matchEvents → filters → field extraction → envelope.
	decision := inbound.Decide(body, r.Header, requestID, agentName, binding)
	if !decision.Match {
		slog.Info("inbound: dropped",
			"agent", agentName, "binding", bindingName,
			"reason", decision.DropReason, "request_id", requestID)
		s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
			inboundOutcomeDropped, decision.DropReason, "")
		// Per the matrix: both unmatched-event and filter-rejected are
		// "the operator deliberately said no thanks" outcomes. Surface
		// them as Normal Events so a human watching `kubectl describe
		// agent` can see that filtering is working as configured. They're
		// per-occurrence (low-volume by nature — filter configuration
		// determines fire rate, not request rate).
		switch decision.DropReason {
		case inbound.DropReasonUnmatchedEvent:
			s.emitInboundEvent(agent, corev1.EventTypeNormal, inboundEventUnmatched,
				"binding=%s dropped event not in matchEvents (request_id=%s)",
				bindingName, requestID)
		case inbound.DropReasonFilterRejected:
			s.emitInboundEvent(agent, corev1.EventTypeNormal, inboundEventFilterRejected,
				"binding=%s dropped by filters (request_id=%s)",
				bindingName, requestID)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      "dropped",
			"drop_reason": decision.DropReason,
			"request_id":  requestID,
		})
		return
	}

	// Stash the rendered envelope so the operator-facing replay endpoint
	// can re-fire it later. Done BEFORE the rate-limit and queue-full
	// checks so a request that runs into a temporary capacity wall is
	// still replayable once the operator has resolved the cause —
	// re-driving these failures is precisely what the replay endpoint
	// exists for.
	//
	// Cache failure is non-fatal: log + continue. Replay won't work for
	// this run, but dispatch is unaffected.
	if s.InboundEnvelopeCache != nil {
		cacheCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		if err := s.InboundEnvelopeCache.Put(cacheCtx, requestID, decision.Envelope); err != nil {
			slog.Warn("inbound: envelope cache write failed",
				"agent", agentName, "binding", bindingName,
				"request_id", requestID, "error", err)
		}
		cancel()
	}

	// Per-binding rate limit. Allow returns false if the bucket is empty.
	maxPerMinute := 0
	if binding.Limits != nil {
		maxPerMinute = binding.Limits.MaxPerMinute
	}
	if s.InboundRateLimiter != nil && !s.InboundRateLimiter.Allow(agentName, bindingName, maxPerMinute) {
		slog.Info("inbound: rate-limited",
			"agent", agentName, "binding", bindingName,
			"max_per_minute", maxPerMinute, "request_id", requestID)
		// Per-occurrence audit entry — operators DO want to see every
		// rate-limited request in the ring buffer for capacity planning.
		// But the k8s Event is aggregated (1/min) because rate-limit
		// trips fire at high frequency by definition.
		s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
			inboundOutcomeDropped, inboundDropRateLimited, "")
		if s.InboundEventAggregator != nil {
			s.InboundEventAggregator.Increment(agentName, inboundEventRateLimitTrip,
				fmt.Sprintf("rate limit exceeded on binding=%s", bindingName))
		}
		writeJSONError(w, http.StatusTooManyRequests, "rate_limited",
			"rate limit exceeded for binding")
		return
	}

	// Queue. Enqueue is non-blocking; ErrQueueFull means the per-agent
	// channel is at capacity and the caller should retry later.
	if s.InboundQueue == nil {
		slog.Error("inbound: queue not configured",
			"agent", agentName, "binding", bindingName, "request_id", requestID)
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"inbound dispatch not configured on this control plane")
		return
	}
	job := inbound.Job{
		Agent:      agentName,
		Binding:    bindingName,
		RequestID:  requestID,
		Envelope:   decision.Envelope,
		EnqueuedAt: time.Now().UTC(),
	}
	if err := s.InboundQueue.Enqueue(job); err != nil {
		if errors.Is(err, inbound.ErrQueueFull) {
			slog.Warn("inbound: queue full",
				"agent", agentName, "binding", bindingName, "request_id", requestID)
			s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
				inboundOutcomeDropped, inboundDropQueueFull, "")
			if s.InboundEventAggregator != nil {
				s.InboundEventAggregator.Increment(agentName, inboundEventQueueFull,
					fmt.Sprintf("inbound queue saturated on binding=%s", bindingName))
			}
			writeJSONError(w, http.StatusTooManyRequests, "queue_full",
				"agent inbound queue is full; retry later")
			return
		}
		slog.Error("inbound: enqueue failed",
			"agent", agentName, "binding", bindingName,
			"error", err, "request_id", requestID)
		// An unexpected enqueue error isn't covered by the matrix as a
		// per-occurrence Event, but it IS worth recording in the audit
		// ring buffer so operators can see it on the Agent CR. We use
		// "queue-full" as the closest category — it's still an enqueue
		// failure, just one we didn't anticipate.
		s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
			inboundOutcomeDropped, inboundDropQueueFull, err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal_error",
			"failed to enqueue inbound job")
		return
	}

	slog.Info("inbound: dispatched",
		"agent", agentName, "binding", bindingName, "request_id", requestID)
	// FinishedAt = enqueue time. The receiver doesn't see exec result —
	// that's a Phase 2 concern (kyber#208 follow-ups). For now the audit
	// entry says "we accepted and queued it"; the Phase 2 work will
	// update FinishedAt + Outcome from the queue handler.
	s.recordInboundRun(r.Context(), agentName, bindingName, requestID, &startedAt,
		inboundOutcomeDispatched, "", "")
	s.emitInboundEvent(agent, corev1.EventTypeNormal, inboundEventDispatched,
		"binding=%s dispatched (request_id=%s)", bindingName, requestID)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "dispatched",
		"request_id": requestID,
	})
}
