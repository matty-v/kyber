package api

import (
	"bufio"
	"context"
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
	n, err := w.responseWriter.Write(b)
	if err != nil {
		w.failed = true
	}
	return n, err
}
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
				case state.outcome != "":
					result.Outcome = state.outcome
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
	action := string(scope)
	if action == "" {
		action = "operator:unclassified"
	}
	kind := "agent"
	if scope == ScopeMachineExec || scope == ScopeMachineLogsRead {
		kind = "machine"
	}
	event := securityaudit.Event{CredentialAuthority: "configuration", ID: uuid.NewString(), OperationID: uuid.NewString(), ObservedAt: time.Now().UTC(), Namespace: s.Namespace, RequestID: w.Header().Get(RequestIDHeader), Action: action, ResourceKind: kind, ResourceName: name, Phase: "authorization", Outcome: "deny", Method: auditMethod(r.Method)}
	if event.RequestID == "" {
		event.RequestID = uuid.NewString()
	}
	if caller := callerFrom(r.Context()); caller != nil && caller.PrincipalID != "" && caller.CredentialID != "" && caller.CredentialGeneration > 0 {
		event.PrincipalID = caller.PrincipalID
		event.CredentialID = caller.CredentialID
		event.Generation = caller.CredentialGeneration
	}
	if allowed {
		event.Outcome = "allow"
	}
	state, _ := r.Context().Value(sensitiveAuditContextKey{}).(*sensitiveAuditState)
	if s.SecurityAudit == nil || (allowed && state == nil) {
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
	}
	return true
}

// Record outer-boundary denials without copying unclassified paths or query
// strings into durable history. An unaffiliated/old key may lack stable IDs;
// represent that as unknown attribution rather than inventing an identity.
func (s *Server) auditOperatorDenial(w http.ResponseWriter, r *http.Request, action, kind, name string) {
	if s.SecurityAudit == nil {
		return
	}
	e := securityaudit.Event{CredentialAuthority: "configuration", ID: uuid.NewString(), OperationID: uuid.NewString(), ObservedAt: time.Now().UTC(), Namespace: s.Namespace, RequestID: w.Header().Get(RequestIDHeader), Action: action, ResourceKind: kind, ResourceName: name, Phase: "authorization", Outcome: "deny", Method: auditMethod(r.Method)}
	if e.RequestID == "" {
		e.RequestID = uuid.NewString()
	}
	if c := callerFrom(r.Context()); c != nil && c.PrincipalID != "" && c.CredentialID != "" && c.CredentialGeneration > 0 {
		e.PrincipalID = c.PrincipalID
		e.CredentialID = c.CredentialID
		e.Generation = c.CredentialGeneration
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
