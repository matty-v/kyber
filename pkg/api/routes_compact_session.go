package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// CompactSessionCooldown is the minimum gap between successive
// compact-session calls for the same agent. Compaction is expensive on the
// runtime side (it costs a model round-trip over the whole conversation), so
// a double-click or a retry loop must not queue several of them.
//
// Longer than RestartSessionCooldown deliberately: a restart is idempotent —
// the second one just kills an already-fresh session — whereas a second
// compact lands as a second "/compact" in the runtime's input queue and
// summarizes the summary.
const CompactSessionCooldown = 60 * time.Second

// lastCompactSessionAt tracks the last successful-claim timestamp per agent.
// Same scope caveat as lastRestartSessionAt: process-local, so the cooldown
// resets across a control-plane restart and is per-replica. Acceptable for
// the same reason (single-replica installs), and with the same migration
// target if that changes.
var (
	lastCompactSessionAt   = map[string]time.Time{}
	lastCompactSessionAtMu sync.Mutex
)

// ResetCompactSessionCooldown drops the in-memory cooldown state. Intended
// for test code that exercises the rate-limit branch repeatedly.
func ResetCompactSessionCooldown() {
	lastCompactSessionAtMu.Lock()
	defer lastCompactSessionAtMu.Unlock()
	lastCompactSessionAt = map[string]time.Time{}
}

// StampCompactSessionForTest sets the last-compact timestamp to now for the
// given agent, so the next call lands in the 429 cooldown branch. Test-only.
func StampCompactSessionForTest(agentName string) {
	lastCompactSessionAtMu.Lock()
	defer lastCompactSessionAtMu.Unlock()
	lastCompactSessionAt[agentName] = time.Now()
}

// handleCompactSession handles POST /api/v1/agents/{name}/compact-session.
//
// Asks the agent's running runtime to compact its own context — summarize
// the conversation so far and continue with a smaller one — without killing
// the session or rolling the pod. The lighter sibling of restart-session:
// restart discards the context, compact reduces it and keeps working.
//
// Guards (identical shape to restart-session, deliberately — the two are
// adjacent buttons and should fail the same way):
//   - 404 if the agent CR is missing
//   - 409 if the agent's phase is not Running (no live session to compact)
//   - 501 if the agent's runtime has no CompactSessionCommand registered
//   - 429 if the last compact for this agent was <CompactSessionCooldown ago
//   - 503 if RestConfig/Clientset aren't wired
//
// On success: returns 200 with the exec stdout+stderr and emits a
// SessionCompacted event on the Agent CR.
//
// "Success" here means the command was DELIVERED to the runtime, not that
// compaction finished — the in-pod script pastes the slash command and
// exits. Compaction then runs on the runtime's own clock and can take
// minutes. The response says so; do not let the UI imply otherwise.
func (s *Server) handleCompactSession(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(),
		types.NamespacedName{Name: agentName, Namespace: s.Namespace}, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return
		}
		slog.Error("failed to get agent for compact-session", "name", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	if agent.Status.Phase != kyberv1.AgentPhaseRunning {
		writeJSONError(w, http.StatusConflict, "agent_not_running",
			fmt.Sprintf("agent '%s' must be Running to compact its session (current phase: %s)", agentName, agent.Status.Phase))
		return
	}

	cmd, ok := s.CompactSessionCommands[agent.Spec.Runtime]
	if !ok || len(cmd) == 0 {
		writeJSONError(w, http.StatusNotImplemented, "not_implemented",
			fmt.Sprintf("compact-session is not supported on runtime '%s' — use restart-session to clear the context instead", agent.Spec.Runtime))
		return
	}

	// Atomic check-and-stamp, same reason as tryClaimRestart: a separate
	// check then stamp lets two concurrent requests both pass the throttle
	// and double-deliver.
	if wait, claimed := tryClaimCompact(agentName, time.Now()); !claimed {
		w.Header().Set("Retry-After", fmt.Sprintf("%.0f", wait.Seconds()+0.5))
		writeJSONError(w, http.StatusTooManyRequests, "cooldown_active",
			fmt.Sprintf("compact-session cooldown active; retry in %.0fs", wait.Seconds()+0.5))
		return
	}

	if s.RestConfig == nil || s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"compact-session not available: restConfig/clientset not configured")
		return
	}

	// 30s, not restart-session's 60s: the script delivers a paste and exits,
	// so anything approaching this bound means the exec itself is wedged,
	// not that compaction is slow.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Reuses execRestartSession — it is a plain "exec this argv in the agent
	// container, no stdin, no TTY" helper with nothing restart-specific in
	// it beyond its name.
	stdout, stderr, err := s.execRestartSession(ctx, "agent-"+agentName, cmd)
	if err != nil {
		slog.Error("compact-session exec failed",
			"agent", agentName, "error", err, "stderr", stderr)

		// The overwhelmingly likely failure on a fleet mid-rollout: the
		// agent is running an image built before this feature, so the
		// in-pod script simply isn't there. Without this branch the
		// operator gets "exec failed: command terminated with exit code 1",
		// which names no cause and points at nothing — and the natural
		// reading ("compaction is broken") is wrong. Say what is actually
		// missing and what fixes it.
		if isMissingInPodScript(stderr) {
			writeJSONError(w, http.StatusNotImplemented, "runtime_image_too_old",
				fmt.Sprintf("agent '%s' is running a runtime image that predates session compaction "+
					"(the in-pod kyber-compact-session script is not installed). Restart the agent onto a "+
					"newer image to enable it; restart-session works in the meantime.", agentName))
			return
		}

		// Otherwise pass the in-pod stderr through. The script's messages
		// are written to be read by an operator ("session restart in
		// progress", "tmux session 'agent' is absent") and are far more
		// useful than the bare exit code.
		writeJSONError(w, http.StatusInternalServerError, "exec_failed",
			fmt.Sprintf("exec failed: %v%s", err, formatExecStderr(stderr)))
		return
	}

	if s.Recorder != nil {
		s.Recorder.Eventf(agent, corev1.EventTypeNormal, "SessionCompacted",
			"compaction requested in-session for runtime=%s", agent.Spec.Runtime)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"agent":   agentName,
		"runtime": agent.Spec.Runtime,
		"stdout":  stdout,
		"stderr":  stderr,
		// Explicit so a caller can't read 200 as "context is now smaller".
		"delivered": true,
		"detail":    "compaction requested; the runtime performs it asynchronously and the context shrinks when it finishes",
	})
}

// maxExecStderrInError bounds how much in-pod stderr is echoed into an API
// error body. The script's own messages are one line; anything much larger
// is a runaway and does not belong in a JSON error.
const maxExecStderrInError = 512

// isMissingInPodScript reports whether stderr says the command itself could
// not be executed, as opposed to the command running and failing.
//
// Matched on the exec wrapper's message rather than an exit code because the
// code is an unhelpful 1: `runuser` reports "failed to execute <path>: No
// such file or directory" and exits 1, indistinguishable by status from a
// script that ran and returned 1.
func isMissingInPodScript(stderr string) bool {
	s := strings.ToLower(stderr)
	if !strings.Contains(s, "kyber-compact-session") {
		return false
	}
	return strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "command not found") ||
		strings.Contains(s, "failed to execute")
}

// IsMissingInPodScriptForTest exposes the stderr classifier to the
// package's external test. Test-only seam; production code calls the
// unexported form directly.
func IsMissingInPodScriptForTest(stderr string) bool { return isMissingInPodScript(stderr) }

// formatExecStderr renders in-pod stderr for an error body, or "" when there
// is nothing worth adding.
func formatExecStderr(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	if len(s) > maxExecStderrInError {
		s = s[:maxExecStderrInError] + "…"
	}
	return " — " + s
}

// tryClaimCompact atomically checks the cooldown window AND stamps the claim
// under one lock. Returns (remaining, false) when still in cooldown (no
// stamp written); (0, true) when the claim was accepted.
//
// now is injected so tests don't race wall-clock ticks.
func tryClaimCompact(agentName string, now time.Time) (time.Duration, bool) {
	lastCompactSessionAtMu.Lock()
	defer lastCompactSessionAtMu.Unlock()
	if last, ok := lastCompactSessionAt[agentName]; ok {
		elapsed := now.Sub(last)
		if elapsed < CompactSessionCooldown {
			return CompactSessionCooldown - elapsed, false
		}
	}
	lastCompactSessionAt[agentName] = now
	return 0, true
}
