package api

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// defaultAgentLogsContainer is the container the agent logs endpoint
// pulls from when the caller doesn't pass ?container=. Matches the
// hard-coded "agent" choice in pkg/api/routes_restart_session.go:177
// (the canonical exec-style endpoint pattern) — the runtime container,
// not a sidecar.
const defaultAgentLogsContainer = "agent"

// agentLogsAllowedContainers pins the operator-tailable container set on
// an agent pod. The set is intentionally hardcoded rather than discovered
// from the live pod spec: the container layout is a stable contract
// (one runtime container plus the kyber-system sidecars) and discovery
// would invite drift between "what the apiserver accepts" and "what
// kyber surfaces as a usable container value" without making the
// operator experience any better. Adding a new container is a deliberate
// edit here paired with the pod-builder change.
//
// Why each is listed:
//   - "agent"               — the Claude Code runtime container (default)
//   - "session-brief"       — the brief-writer sidecar that hydrates
//     ~/CLAUDE.md from the brief store on boot
//   - "kyber-status-sidecar" — activity-state + heartbeat reporter
//     (kyber#247 / #248)
var agentLogsAllowedContainers = map[string]struct{}{
	"agent":                {},
	"session-brief":        {},
	"kyber-status-sidecar": {},
}

// handleAgentLogs streams pod logs for a named Agent.
//
// GET /api/v1/agents/{name}/logs
//
// Query parameters:
//
//	follow    bool   — stream logs continuously (default false)
//	tail      int    — number of lines from the end (default 100)
//	since     string — Go duration; start that long ago (e.g. "5m", "1h")
//	previous  bool   — return logs from the previously-terminated container
//	container string — which container to tail (default "agent"; one of
//	                   "agent", "session-brief", "kyber-status-sidecar").
//	                   Unknown values return 400 invalid_container.
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent for logs", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	// kyber#431: branch on ?source= AFTER the agent-existence check (so 404 and
	// 405 behave identically across sources) but BEFORE any kubelet-specific
	// logic. source=archive reads the durable GCS-backed surface for an absolute
	// RFC3339 window; source omitted or =kubelet leaves the live-tail path below
	// byte-for-byte unchanged.
	switch r.URL.Query().Get("source") {
	case "", "kubelet":
		// fall through to the kubelet live-tail path below.
	case "archive":
		s.handleAgentLogsArchive(w, r, name)
		return
	case "transcript":
		// kyber#446: the agent's Claude Code session JSONL, shipped by the
		// transcript-tailer sidecar to the transcripts/ object lane. Same
		// absolute-window read machinery as archive, different object root.
		s.handleAgentLogsTranscript(w, r, name)
		return
	default:
		writeJSONError(w, http.StatusBadRequest, "invalid_source",
			"source must be 'kubelet' (default), 'archive', or 'transcript'")
		return
	}

	// Pod name is deterministic: "agent-<name>".
	// Kept inline to avoid pulling pkg/controllers/agent into the API layer.
	podName := "agent-" + name

	if s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"log streaming not available: clientset not configured")
		return
	}

	// kyber#385: agent pods carry three containers; the kube-apiserver
	// rejects log requests that don't name one. Validate the operator's
	// choice (or fall back to "agent") before opening the stream.
	container, ok := parseAgentLogsContainer(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_container",
			"container must be one of: agent, session-brief, kyber-status-sidecar")
		return
	}

	opts := parsePodLogOptions(r)
	opts.Container = container
	req := s.Clientset.CoreV1().Pods(s.Namespace).GetLogs(podName, opts)
	stream, err := req.Stream(r.Context())
	if err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found",
				"agent '"+name+"' has no running pod")
			return
		}
		// Keep the upstream error server-side; the public body stays generic so
		// it can't leak internal cluster/stream detail. (kyber#434)
		slog.Error("failed to open agent log stream", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "stream_error",
			"failed to open log stream")
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	// Write a single empty line + flush to force the HTTP/2 HEADERS frame (and
	// the first DATA frame) through to the client immediately. Without at least
	// one body write, Go's HTTP/2 server holds the HEADERS frame until the
	// handler exits or the first body byte arrives — so http.Client.Do() blocks
	// indefinitely on a quiet pod with tail=0.
	if _, err := w.Write([]byte("\n")); err != nil {
		return
	}
	if canFlush {
		flusher.Flush()
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				// Client disconnected.
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				// Stream broken — we've already started writing, so just stop.
				_ = err
			}
			return
		}
	}
}

// handleMachineLogs streams logs from the node-agent pod for a named Machine.
//
// GET /api/v1/machines/{name}/logs
//
// The node-agent pod is identified by the label selector
// "app.kubernetes.io/name=node-agent,kyber.io/machine=<name>".
// If no such pod is found, 404 is returned.
func (s *Server) handleMachineLogs(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	machine := &kyberv1.Machine{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, machine); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "machine '"+name+"' not found")
			return
		}
		slog.Error("failed to get machine for logs", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get machine")
		return
	}

	if s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"log streaming not available: clientset not configured")
		return
	}

	// Find the node-agent pod for this machine.
	podList, err := s.Clientset.CoreV1().Pods(s.Namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=node-agent,kyber.io/machine=" + name,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list pods")
		return
	}
	if len(podList.Items) == 0 {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"no node-agent pod found for machine '"+name+"'")
		return
	}

	podName := podList.Items[0].Name
	opts := parsePodLogOptions(r)
	req := s.Clientset.CoreV1().Pods(s.Namespace).GetLogs(podName, opts)
	stream, err := req.Stream(r.Context())
	if err != nil {
		status := http.StatusInternalServerError
		if k8serrors.IsNotFound(err) {
			status = http.StatusNotFound
		}
		// Keep the upstream error server-side; the public body stays generic so
		// it can't leak internal cluster/stream detail. (kyber#434)
		slog.Error("failed to open machine log stream", "name", name, "error", err)
		writeJSONError(w, status, "stream_error", "failed to open log stream")
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	// Write a single empty line + flush to force the HTTP/2 HEADERS + first DATA
	// frame through to the client immediately.
	if _, err := w.Write([]byte("\n")); err != nil {
		return
	}
	if canFlush {
		flusher.Flush()
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// parseAgentLogsContainer extracts the ?container= query param for the
// agent logs endpoint. Empty input falls back to defaultAgentLogsContainer
// ("agent"). Returns (container, true) on success or ("", false) when
// the supplied value is not in agentLogsAllowedContainers. (kyber#385)
//
// Validation lives here rather than in parsePodLogOptions because the
// allowed-set is agent-pod-specific — the machine-logs path shares the
// generic options parser but the node-agent pod is single-container and
// doesn't need (or want) this gate.
func parseAgentLogsContainer(r *http.Request) (string, bool) {
	c := r.URL.Query().Get("container")
	if c == "" {
		return defaultAgentLogsContainer, true
	}
	if _, ok := agentLogsAllowedContainers[c]; ok {
		return c, true
	}
	return "", false
}

// parsePodLogOptions extracts log query parameters from the request.
func parsePodLogOptions(r *http.Request) *corev1.PodLogOptions {
	q := r.URL.Query()
	opts := &corev1.PodLogOptions{}

	// follow
	if strings.EqualFold(q.Get("follow"), "true") || q.Get("follow") == "1" {
		opts.Follow = true
	}

	// tail (default 100)
	tail := int64(100)
	if v := q.Get("tail"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			tail = n
		}
	}
	opts.TailLines = &tail

	// since (Go duration, e.g. "5m")
	if v := q.Get("since"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			t := metav1.NewTime(time.Now().UTC().Add(-d))
			opts.SinceTime = &t
		}
	}

	// previous container
	if strings.EqualFold(q.Get("previous"), "true") || q.Get("previous") == "1" {
		opts.Previous = true
	}

	return opts
}

// parseArchiveWindow extracts the absolute RFC3339 window for source=archive.
// Unlike the kubelet path's relative-duration `since`, both `since` and `until`
// here are absolute RFC3339 timestamps and both are required. Returns a non-empty
// errMsg (and zero times) on any malformed/absent bound or when until < since;
// the caller maps a non-empty errMsg to HTTP 400 invalid_window. This mirrors
// the (value, ok) validation style of parseAgentLogsContainer.
func parseArchiveWindow(r *http.Request) (since, until time.Time, errMsg string) {
	q := r.URL.Query()
	sv := q.Get("since")
	uv := q.Get("until")
	if sv == "" || uv == "" {
		return time.Time{}, time.Time{},
			"source=archive requires both 'since' and 'until' as RFC3339 timestamps"
	}
	s, err := time.Parse(time.RFC3339, sv)
	if err != nil {
		return time.Time{}, time.Time{}, "'since' must be an RFC3339 timestamp (e.g. 2026-06-03T10:00:00Z)"
	}
	u, err := time.Parse(time.RFC3339, uv)
	if err != nil {
		return time.Time{}, time.Time{}, "'until' must be an RFC3339 timestamp (e.g. 2026-06-03T11:00:00Z)"
	}
	if u.Before(s) {
		return time.Time{}, time.Time{}, "'until' must not be earlier than 'since'"
	}
	return s, u, ""
}

// handleAgentLogsArchive serves GET /api/v1/agents/{name}/logs?source=archive.
// It reads the named agent's durable, off-cluster log lines for the absolute
// RFC3339 window [since, until] from the ArchiveReader and streams them as
// text/plain — the same wire shape as the kubelet live-tail, so the PWA's
// newline reader consumes both identically.
//
// Preconditions already satisfied by the caller (handleAgentLogs): method is
// GET and the agent CRD exists (else 404). Auth is inherited from the protected
// mux, so an unauthenticated request never reaches here.
func (s *Server) handleAgentLogsArchive(w http.ResponseWriter, r *http.Request, name string) {
	s.serveWindowedLines(w, r, name, s.ArchiveReader, s.ArchiveDisabledReason, "archive")
}

// handleAgentLogsTranscript serves GET /api/v1/agents/{name}/logs?source=transcript.
// It returns the named agent's Claude Code session JSONL for the absolute RFC3339
// window [since, until], read from the transcript lane (transcripts/ object root)
// via the TranscriptReader. Identical window/auth/404 semantics to source=archive
// — the only difference is the object root and the secret-bearing data class.
//
// Preconditions already satisfied by the caller (handleAgentLogs): method is GET
// and the agent CRD exists (else 404). Auth is inherited from the protected mux,
// so an unauthenticated request never reaches here and authz is identical to
// source=archive — important because this surface exposes full session content.
func (s *Server) handleAgentLogsTranscript(w http.ResponseWriter, r *http.Request, name string) {
	s.serveWindowedLines(w, r, name, s.TranscriptReader, s.TranscriptDisabledReason, "transcript")
}

// serveWindowedLines is the shared windowed-read handler behind both
// source=archive and source=transcript. The two surfaces differ ONLY in which
// reader and disabled-reason they pass and the log/surface label — folding them
// here keeps source=archive and source=transcript provably in lockstep (same
// 400 invalid_window, 503 service_unavailable, 502 archive_read_error, and
// text/plain streaming shape) so neither can drift from the other.
func (s *Server) serveWindowedLines(w http.ResponseWriter, r *http.Request, name string, reader ArchiveReader, disabledReason, surface string) {
	since, until, errMsg := parseArchiveWindow(r)
	if errMsg != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_window", errMsg)
		return
	}

	if reader == nil {
		// Enrich the 503 to name the missing config keys (never their values) so
		// a misconfig is self-diagnosing (kyber#437). The disabled reason is set
		// at startup from the same config the reader is built from.
		msg := surface + " log surface not configured"
		if disabledReason != "" {
			msg = msg + ": " + disabledReason
		}
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", msg)
		return
	}

	// kyber#463: bound AGGREGATE in-flight reads. ReadAgentLines below builds the
	// (per-read-capped, kyber#456) result in memory and parses NDJSON — the
	// memory- and CPU-heavy step. Without a cap, N concurrent large reads
	// collectively exhaust the CP or starve its CPU, timing out the liveness probe
	// → SIGKILL of the whole process (exit 137) and a 502 cascade on unrelated
	// endpoints. Acquire a slot HERE — after the cheap parse/nil checks, BEFORE any
	// heavy work or byte written — so an over-cap read returns a clean 429 with no
	// partial body. The slot is released via defer on EVERY return path below
	// (including the client-disconnect early-return in the stream loop), so a
	// dropped client can't leak a slot and wedge the cap. The gate wraps only this
	// shared archive+transcript handler, so /version, the agents list, and the
	// :8081 probe listener are never throttled (endpoint isolation).
	release, ok := s.tryAcquireReadSlot()
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeJSONError(w, http.StatusTooManyRequests, "too_many_concurrent_reads",
			"too many concurrent log reads in flight; retry shortly")
		return
	}
	defer release()

	result, err := reader.ReadAgentLines(r.Context(), name, since, until)
	if err != nil {
		// Keep the full upstream error (which can carry bucket/object/project
		// identifiers) server-side only; the public body stays generic. (kyber#434)
		slog.Error("windowed log read failed", "name", name, "surface", surface, "error", err)
		writeJSONError(w, http.StatusBadGateway, "archive_read_error",
			"failed to read archived logs")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// kyber#455: a windowed read is memory-bounded and may be capped. When it is,
	// signal it explicitly via a response header (set before WriteHeader, so it's
	// always visible to the caller) rather than returning a silent partial. The
	// body stays a plain newline-delimited stream so the existing PWA reader is
	// unchanged; a caller that cares reads the header. Documented in
	// docs/architecture/log-retention.md.
	if result.Truncated {
		w.Header().Set("X-Kyber-Log-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	for _, l := range result.Lines {
		if _, err := io.WriteString(w, l.Text+"\n"); err != nil {
			// Client disconnected.
			return
		}
		if canFlush {
			flusher.Flush()
		}
	}
}
