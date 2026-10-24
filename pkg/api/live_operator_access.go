package api

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

const operatorAccessCheckInterval = 3 * time.Second
const operatorAccessCheckTimeout = 2 * time.Second
const operatorStreamWriteTimeout = 3 * time.Second
const operatorWebSocketCloseGrace = 200 * time.Millisecond

var errOperatorAccessChanged = errors.New("operator access changed")

type liveOperatorContextKey struct{}

type liveOperatorAccess struct {
	mu        sync.Mutex
	checks    []func(*Caller) bool
	admission *securityaudit.Event
	requestID string
}

func (s *liveOperatorAccess) permits(caller *Caller) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, check := range s.checks {
		if !check(caller) {
			return false
		}
	}
	return true
}
func registerLiveOperatorAccess(r *http.Request, check func(*Caller) bool, admission *securityaudit.Event) bool {
	state, _ := r.Context().Value(liveOperatorContextKey{}).(*liveOperatorAccess)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if check != nil {
		state.checks = append(state.checks, check)
	}
	if admission != nil {
		event := *admission
		state.admission = &event
	}
	return true
}

// liveOperatorRequests is inside authentication, so the initial identity has
// already been verified. No credential snapshot is reused for later checks.
// Short operations finish before a tick; long operations cannot retain a
// permission snapshot until their connection or browser cookie expires.
func (s *Server) liveOperatorRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		state := &liveOperatorAccess{requestID: w.Header().Get(RequestIDHeader)}
		r = r.WithContext(context.WithValue(ctx, liveOperatorContextKey{}, state))
		before := callerFrom(r.Context())
		writer := &liveOperatorWriter{ResponseWriter: w, ctx: r.Context()}
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(operatorAccessCheckInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				if ctx.Err() != nil {
					return
				}
				checkCtx, stop := context.WithTimeout(r.Context(), operatorAccessCheckTimeout)
				var after *Caller
				var err error
				if s.auth == nil {
					err = errOperatorAuthorityUnavailable
				} else {
					after, err = s.auth.Authenticate(r.WithContext(checkCtx))
				}
				stop()
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, errOperatorAuthorityUnavailable) {
					cancel(errOperatorAuthorityUnavailable)
					interruptOperatorTransport(w, r)
					return
				}
				allowed := err == nil && before != nil && after != nil && sameCredentialIdentity(*before, *after) && (after.Scopes.full || s.restrictedOperatorRoute(r, after)) && state.permits(after)
				if allowed {
					continue
				}
				cancel(errOperatorAccessChanged)
				// Interrupt blocked writes immediately; do not wait for the audit store
				// before closing access. New writes also observe the cancelled context.
				interruptOperatorTransport(w, r)
				s.auditLiveOperatorDenial(r, state)
				return
			}
		}()
		defer func() {
			cancel(nil)
			<-done
			cause := context.Cause(r.Context())
			if !errors.Is(cause, errOperatorAccessChanged) && !errors.Is(cause, errOperatorAuthorityUnavailable) {
				// Give net/http a fresh bounded deadline for its final buffered bytes,
				// including after a long idle stream or bounded completion auditing.
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(operatorStreamWriteTimeout))
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

func (s *Server) auditLiveOperatorDenial(r *http.Request, state *liveOperatorAccess) {
	state.mu.Lock()
	var admission *securityaudit.Event
	if state.admission != nil {
		event := *state.admission
		admission = &event
	}
	state.mu.Unlock()
	if admission == nil {
		s.auditOperatorDenialForRequest(r.WithContext(context.WithoutCancel(r.Context())), state.requestID, "operator:unclassified", "installation", "")
		return
	}
	event := *admission
	event.ID = uuid.NewString()
	event.ObservedAt = time.Now().UTC()
	event.Phase, event.Outcome = "authorization", "deny"
	if s.SecurityAudit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), securityAuditTimeout)
	defer cancel()
	// A denial remains a denial even if collection is unavailable. Never emit
	// transport errors, keys, request paths or command arguments in diagnostics.
	if _, err := s.SecurityAudit.Append(ctx, event); err != nil {
		slog.Error("security audit live-access denial unavailable", "operation_id", event.OperationID, "action", event.Action)
	}
}

// Stop blocked request-body reads as well as writes. Closing access must not
// wait for a slow uploader to send another byte. Standard server transports
// support deadlines; optional wrappers are reached through Unwrap.
func interruptOperatorTransport(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now())
	if controller.SetReadDeadline(time.Now()) == nil && r.Body != nil {
		_ = r.Body.Close()
	}
}

type liveOperatorWriter struct {
	http.ResponseWriter
	ctx context.Context
}

func (w *liveOperatorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *liveOperatorWriter) ready() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(operatorStreamWriteTimeout))
	return nil
}
func (w *liveOperatorWriter) prepareHeaders() {
	if w.ResponseWriter.Header().Get("Content-Type") == "" {
		w.ResponseWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.ResponseWriter.Header().Set("X-Content-Type-Options", "nosniff")
}
func (w *liveOperatorWriter) Write(b []byte) (int, error) {
	if err := w.ready(); err != nil {
		return 0, err
	}
	w.prepareHeaders()
	return w.ResponseWriter.Write(b)
}
func (w *liveOperatorWriter) WriteHeader(status int) {
	// Keep logical status reporting for an ordinary disconnected client, while
	// never releasing headers after an explicit live-access cancellation.
	cause := context.Cause(w.ctx)
	if errors.Is(cause, errOperatorAccessChanged) || errors.Is(cause, errOperatorAuthorityUnavailable) {
		return
	}
	if w.ctx.Err() != nil || w.ready() == nil {
		w.prepareHeaders()
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *liveOperatorWriter) Flush() {
	if w.ready() != nil {
		return
	}
	w.prepareHeaders()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *liveOperatorWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, nil, err
	}
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("websocket transport unavailable")
	}
	conn, buffer, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	// The protocol handler has a short opportunity to send a close frame.
	// Closing the socket independently also interrupts a backpressured writer
	// or a handler waiting on remote I/O. Completion always cancels this context.
	go func() {
		<-w.ctx.Done()
		timer := time.NewTimer(operatorWebSocketCloseGrace)
		defer timer.Stop()
		<-timer.C
		_ = conn.Close()
	}()
	return conn, buffer, nil
}
