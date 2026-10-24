package api

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

const securityAuditTimeout = 2 * time.Second

type sensitiveAuditContextKey struct{}
type sensitiveAuditState struct {
	admission *securityaudit.Event
	outcome   string
}

type sensitiveAuditWriter struct {
	*responseWriter
	failed   bool
	hijacked bool
}

func (w *sensitiveAuditWriter) Write(b []byte) (int, error) {
	// Protected API payloads with no declared type are text, never browser-
	// sniffed markup. Preserve explicitly declared JSON/binary/image responses.
	if w.responseWriter.Header().Get("Content-Type") == "" {
		w.responseWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.responseWriter.Header().Set("X-Content-Type-Options", "nosniff")
	n, err := w.responseWriter.Write(b)
	if err != nil {
		w.failed = true
	}
	return n, err
}
func (w *sensitiveAuditWriter) prepareHeaders() {
	if w.responseWriter.Header().Get("Content-Type") == "" {
		w.responseWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.responseWriter.Header().Set("X-Content-Type-Options", "nosniff")
}
func (w *sensitiveAuditWriter) WriteHeader(status int) {
	w.prepareHeaders()
	w.responseWriter.WriteHeader(status)
}
func (w *sensitiveAuditWriter) Flush() { w.prepareHeaders(); w.responseWriter.Flush() }

func (w *sensitiveAuditWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, r, err := w.responseWriter.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return c, r, err
}

// Sensitive handlers admit an operation only after its authorization decision
// has been committed. The wrapper persists a separate outcome; it never treats
// a WebSocket handshake as proof that a command completed successfully.
func (s *Server) auditSensitiveRequests(next http.Handler) http.Handler {
	return s.liveOperatorRequests(s.auditOperatorOutcomes(next))
}

// Public capability routes use this collector inside their narrow live guard.
func (s *Server) auditOperatorOutcomes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &sensitiveAuditState{}
		r = r.WithContext(context.WithValue(r.Context(), sensitiveAuditContextKey{}, state))
		observed := &sensitiveAuditWriter{responseWriter: newResponseWriter(w)}
		completed := false
		defer func() {
			if state.admission == nil {
				return
			}
			result := *state.admission
			result.ID = uuid.NewString()
			result.ObservedAt = time.Now().UTC()
			result.Phase = "completion"
			result.Outcome = "unknown"
			if completed {
				switch {
				// A confirmed mutation remains committed even if its response is lost
				// or access changes afterward. Separate denials describe closed access.
				case state.outcome != "":
					result.Outcome = state.outcome
				case r.Context().Err() != nil && securityActionMayMutate(result.Action):
					result.Outcome = "unknown"
				case errors.Is(context.Cause(r.Context()), errOperatorAuthorityUnavailable):
					result.Outcome = "unknown"
				case errors.Is(context.Cause(r.Context()), errOperatorAccessChanged):
					result.Outcome = "failure"
				case observed.status >= 400:
					result.Outcome = "failure"
				case observed.failed || r.Context().Err() != nil:
					result.Outcome = "unknown"
				case !observed.hijacked:
					result.Outcome = "success"
				}
			}
			// Client disconnect must not cancel the record of what happened. Keep the
			// write bounded and retain correlation, not request bodies or error text.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), securityAuditTimeout)
			defer cancel()
			if _, err := s.SecurityAudit.Append(ctx, result); err != nil {
				slog.Error("security audit completion unavailable", "operation_id", result.OperationID, "action", result.Action, "resource_kind", result.ResourceKind, "resource", result.ResourceName)
			}
		}()
		next.ServeHTTP(observed, r)
		completed = true
	})
}

func markSensitiveOutcome(r *http.Request, outcome string) {
	if state, _ := r.Context().Value(sensitiveAuditContextKey{}).(*sensitiveAuditState); state != nil {
		state.outcome = outcome
	}
}

func (s *Server) auditSensitiveDecision(w http.ResponseWriter, r *http.Request, name string, scope Scope, allowed bool) bool {
	kind := "agent"
	if scope == ScopeMachineExec || scope == ScopeMachineLogsRead {
		kind = "machine"
	}
	return s.auditSensitiveResourceDecision(w, r, name, scope, kind, allowed)
}

func (s *Server) auditSensitiveResourceDecision(w http.ResponseWriter, r *http.Request, name string, scope Scope, kind string, allowed bool) bool {
	check := func(current *Caller) bool {
		if kind == "installation" {
			return current.Scopes.full
		}
		resources := current.AgentResources
		if kind == "machine" {
			resources = current.MachineResources
		}
		return current.Scopes.full || scope != "" && current.Scopes.Has(scope) && resources.Has(s.Namespace+"/"+name)
	}
	return s.auditRequiredOperatorDecision(w, r, string(scope), kind, name, allowed, check)
}

// Management actions and capabilities supply their actual permission predicate,
// rather than treating every installation target as administrator-only.
func (s *Server) auditRequiredOperatorDecision(w http.ResponseWriter, r *http.Request, action, kind, name string, allowed bool, check func(*Caller) bool) bool {
	if action == "" {
		action = "operator:unclassified"
	}
	event := securityaudit.Event{CredentialAuthority: "configuration", ID: uuid.NewString(), OperationID: uuid.NewString(), ObservedAt: time.Now().UTC(), Namespace: s.Namespace, RequestID: w.Header().Get(RequestIDHeader), Action: action, ResourceKind: kind, ResourceName: name, Phase: "authorization", Outcome: "deny", Method: auditMethod(r.Method)}
	if event.RequestID == "" {
		event.RequestID = uuid.NewString()
	}
	if caller := callerFrom(r.Context()); caller != nil && caller.PrincipalID != "" && caller.CredentialID != "" && caller.CredentialGeneration > 0 {
		event.PrincipalID = caller.PrincipalID
		event.CredentialID = caller.CredentialID
		event.Generation = caller.CredentialGeneration
		if caller.CredentialAuthority == "registry" {
			event.CredentialAuthority = "registry"
		}
	}
	if allowed {
		event.Outcome = "allow"
	}
	state, _ := r.Context().Value(sensitiveAuditContextKey{}).(*sensitiveAuditState)
	liveState, _ := r.Context().Value(liveOperatorContextKey{}).(*liveOperatorAccess)
	if s.SecurityAudit == nil || (allowed && (state == nil || liveState == nil)) {
		if allowed {
			writeJSONError(w, http.StatusServiceUnavailable, "security_audit_unavailable", "security history is unavailable; this operation was not started")
		}
		return !allowed
	}
	ctx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	if _, err := s.SecurityAudit.Append(ctx, event); err != nil {
		slog.Error("security audit decision unavailable", "operation_id", event.OperationID, "action", event.Action, "decision", event.Outcome)
		if allowed {
			writeJSONError(w, http.StatusServiceUnavailable, "security_audit_unavailable", "security history is unavailable; this operation was not started")
		}
		return !allowed
	}
	if allowed {
		state.admission = &event
		registerLiveOperatorAccess(r, check, &event)
	}
	return true
}

// Record outer-boundary denials without copying unclassified paths or query
// strings into durable history. An unaffiliated/old key may lack stable IDs;
// represent that as unknown attribution rather than inventing an identity.
func (s *Server) auditOperatorDenial(w http.ResponseWriter, r *http.Request, action, kind, name string) {
	s.auditOperatorDenialForRequest(r, w.Header().Get(RequestIDHeader), action, kind, name)
}

func (s *Server) auditOperatorDenialForRequest(r *http.Request, requestID, action, kind, name string) {
	if s.SecurityAudit == nil {
		return
	}
	e := securityaudit.Event{CredentialAuthority: "configuration", ID: uuid.NewString(), OperationID: uuid.NewString(), ObservedAt: time.Now().UTC(), Namespace: s.Namespace, RequestID: requestID, Action: action, ResourceKind: kind, ResourceName: name, Phase: "authorization", Outcome: "deny", Method: auditMethod(r.Method)}
	if e.RequestID == "" {
		e.RequestID = uuid.NewString()
	}
	if c := callerFrom(r.Context()); c != nil && c.PrincipalID != "" && c.CredentialID != "" && c.CredentialGeneration > 0 {
		e.PrincipalID = c.PrincipalID
		e.CredentialID = c.CredentialID
		e.Generation = c.CredentialGeneration
		if c.CredentialAuthority == "registry" {
			e.CredentialAuthority = "registry"
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	if _, err := s.SecurityAudit.Append(ctx, e); err != nil {
		slog.Error("security audit denial unavailable", "operation_id", e.OperationID, "action", e.Action)
	}
}

func auditMethod(method string) string {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return method
	default:
		return "OTHER"
	}
}

func (s *Server) requirePlatformLogAudit(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireInstallationAdmin(w, r, "read-platform-logs") {
		return false
	}
	return s.auditSensitiveResourceDecision(w, r, "", ScopeLogsRead, "installation", true)
}

// Add a target decision after discovery has matched trusted backend metadata,
// before opening content. Both decisions share an operation, so a successful
// lookup is never confused with a second independent read or an incomplete job.
func (s *Server) auditPlatformLogTarget(w http.ResponseWriter, r *http.Request, selection GenericArchiveSelection) bool {
	state, _ := r.Context().Value(sensitiveAuditContextKey{}).(*sensitiveAuditState)
	if state == nil || state.admission == nil || s.SecurityAudit == nil {
		writeJSONError(w, 503, "security_audit_unavailable", "security history is unavailable; content was not read")
		return false
	}
	e := *state.admission
	e.ID = uuid.NewString()
	e.ObservedAt = time.Now().UTC()
	e.ResourceKind = "log-container"
	e.ResourceName = selection.PodUID
	e.Container = selection.Container
	e.LogComponent = selection.Component
	e.LogWorkload = selection.Workload
	ctx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	if _, err := s.SecurityAudit.Append(ctx, e); err != nil {
		slog.Error("security audit target decision unavailable", "operation_id", e.OperationID, "resource", e.ResourceName)
		writeJSONError(w, 503, "security_audit_unavailable", "security history is unavailable; content was not read")
		return false
	}
	state.admission = &e
	registerLiveOperatorAccess(r, nil, &e)
	return true
}

// A cancelled mutation without a confirmed acknowledgement may have committed
// remotely. Closing access cannot be described as rolling that effect back.
func securityActionMayMutate(action string) bool {
	switch action {
	case "secrets:write", "credential:create", "credential:update", "credential:rotate", "credential:revoke", "credential:recover", "lifecycle:write", "lifecycle:delete", "archives:export", "archives:import", "archives:upload", "archives:cancel", "archives:delete":
		return true
	default:
		return false
	}
}
