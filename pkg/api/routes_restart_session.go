package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// RestartSessionCooldown is the minimum gap between successive
// restart-session calls for the same agent. A runtime crash-loop caused by
// a bad launch script would otherwise spin without backoff; the cooldown
// gives operators a chance to notice before the hundredth try.
const RestartSessionCooldown = 10 * time.Second

// lastRestartSessionAt tracks the last successful-claim timestamp per agent.
// Keyed by agent name; guarded by lastRestartSessionAtMu. Scope is
// process-local — the cooldown resets across a control-plane restart, and
// with multiple API replicas each replica has its own map. Acceptable
// today (production installs run a single replica); the migration target when
// that changes is a shared store (Redis / the same tokenstore layer).
var (
	lastRestartSessionAt   = map[string]time.Time{}
	lastRestartSessionAtMu sync.Mutex
)

// ResetRestartSessionCooldown drops the in-memory cooldown state. Intended
// for test code that exercises the rate-limit branch repeatedly.
func ResetRestartSessionCooldown() {
	lastRestartSessionAtMu.Lock()
	defer lastRestartSessionAtMu.Unlock()
	lastRestartSessionAt = map[string]time.Time{}
}

// StampRestartSessionForTest sets the last-restart timestamp to now for the
// given agent, so the next call will land in the 429 cooldown branch. Test-
// only helper — the production path stamps only through tryClaimRestart.
func StampRestartSessionForTest(agentName string) {
	lastRestartSessionAtMu.Lock()
	defer lastRestartSessionAtMu.Unlock()
	lastRestartSessionAt[agentName] = time.Now()
}

// handleRestartSession handles POST /api/v1/agents/{name}/restart-session.
//
// Guards:
//   - 404 if the agent CR is missing
//   - 409 if the agent's phase is not Running (no live session to restart)
//   - 501 if the agent's runtime has no RestartSessionCommand registered
//   - 429 if the last restart for this agent was <RestartSessionCooldown ago
//   - 503 if RestConfig/Clientset aren't wired
//
// On success: runs the runtime-specific command via SPDY exec, emits a
// SessionRestarted event on the Agent CR (when Recorder is wired), and
// returns 200 with the exec stdout+stderr for operator visibility.
func (s *Server) handleRestartSession(w http.ResponseWriter, r *http.Request, agentName string) {
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
		slog.Error("failed to get agent for restart-session", "name", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	if agent.Status.Phase != kyberv1.AgentPhaseRunning {
		writeJSONError(w, http.StatusConflict, "agent_not_running",
			fmt.Sprintf("agent '%s' must be Running to restart its session (current phase: %s)", agentName, agent.Status.Phase))
		return
	}

	cmd, ok := s.RestartSessionCommands[agent.Spec.Runtime]
	if !ok || len(cmd) == 0 {
		writeJSONError(w, http.StatusNotImplemented, "not_implemented",
			fmt.Sprintf("restart-session is not supported on runtime '%s' yet — use restart-pod instead", agent.Spec.Runtime))
		return
	}

	// Atomic claim: if another request stamped the cooldown map between a
	// separate check + stamp, both would otherwise pass the throttle test
	// and double-exec. tryClaimRestart does both under one lock.
	if wait, claimed := tryClaimRestart(agentName, time.Now()); !claimed {
		w.Header().Set("Retry-After", fmt.Sprintf("%.0f", wait.Seconds()+0.5))
		writeJSONError(w, http.StatusTooManyRequests, "cooldown_active",
			fmt.Sprintf("restart-session cooldown active; retry in %.0fs", wait.Seconds()+0.5))
		return
	}

	if s.RestConfig == nil || s.Clientset == nil {
		// Cooldown already claimed for this agent. Fine for the 503 path
		// to hold it — blocks a stampede while operators troubleshoot the
		// missing clientset config.
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"restart-session not available: restConfig/clientset not configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	stdout, stderr, err := s.execRestartSession(ctx, "agent-"+agentName, cmd)
	if err != nil {
		slog.Error("restart-session exec failed",
			"agent", agentName, "error", err, "stderr", stderr)
		writeJSONError(w, http.StatusInternalServerError, "exec_failed",
			fmt.Sprintf("exec failed: %v", err))
		return
	}

	if s.Recorder != nil {
		// SessionRestarted is surfaced in `kubectl describe agent` and
		// anywhere else k8s events are rendered — gives an audit trail
		// without a separate logging path.
		s.Recorder.Eventf(agent, corev1.EventTypeNormal, "SessionRestarted",
			"in-pod tmux session killed and re-launched for runtime=%s", agent.Spec.Runtime)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"agent":   agentName,
		"runtime": agent.Spec.Runtime,
		"stdout":  stdout,
		"stderr":  stderr,
	})
}

// tryClaimRestart atomically checks the cooldown window AND stamps the
// current claim time under one lock. Returns (remaining, false) when the
// agent is still in cooldown (no stamp written); (0, true) when the claim
// was accepted and stamped. Replaces the earlier pair of
// shouldThrottleRestartSession + recordRestartSession — the gap between
// those two calls allowed two parallel requests to both read "not
// throttled" and both proceed.
//
// now is injected so tests don't race wall-clock ticks; production callers
// pass time.Now().
func tryClaimRestart(agentName string, now time.Time) (time.Duration, bool) {
	lastRestartSessionAtMu.Lock()
	defer lastRestartSessionAtMu.Unlock()
	if last, ok := lastRestartSessionAt[agentName]; ok {
		elapsed := now.Sub(last)
		if elapsed < RestartSessionCooldown {
			return RestartSessionCooldown - elapsed, false
		}
	}
	lastRestartSessionAt[agentName] = now
	return 0, true
}

// execRestartSession runs cmd in the given pod via SPDY and returns its
// stdout/stderr. Same shape as execRunJob in routes_agent_jobs.go —
// non-interactive, no stdin, no TTY.
func (s *Server) execRestartSession(ctx context.Context, podName string, cmd []string) (string, string, error) {
	execReq := s.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(s.Namespace).
		SubResource("exec").
		Param("container", "agent").
		Param("stdout", "true").
		Param("stderr", "true")
	for _, c := range cmd {
		execReq = execReq.Param("command", c)
	}

	executor, err := remotecommand.NewSPDYExecutor(s.RestConfig, "POST", execReq.URL())
	if err != nil {
		return "", "", fmt.Errorf("new executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	}); err != nil {
		return stdout.String(), stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// errRestartSessionNotImplemented is the sentinel returned to the caller
// when a runtime doesn't register a RestartSessionCommand. Kept exported-ish
// for tests that want to branch on the specific condition.
var errRestartSessionNotImplemented = errors.New("restart-session not implemented for runtime")

var _ = errRestartSessionNotImplemented // reserved for future richer error typing
