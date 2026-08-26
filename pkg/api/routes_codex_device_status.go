package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/codexauth"
)

// GET /api/v1/agents/{name}/codex-device-auth — what the in-pod device login is
// currently showing.
//
// This is what lets the PWA render the link and the code itself instead of
// making the operator read them out of an embedded terminal. There is no
// machine-readable source for them: `codex login --device-auth` prints a human
// prompt and offers no JSON mode, so the platform reads the tmux pane the flow
// runs in and parses it (pkg/codexauth).
//
// Read-only and idempotent. It never starts, restarts, or cancels a login —
// that is the POST on this same path.
//
// The response is deliberately shaped so that "not ready yet" is a normal 200,
// not an error: a pod that is still booting, a tmux session that exists but has
// not drawn its prompt, and a prompt whose wording we no longer recognise all
// report `starting`. The panel holds a spinner and polls. Errors are reserved
// for the operator having asked the wrong question (wrong runtime, wrong agent)
// or the platform being unable to answer at all.

const (
	// Markers separate our own output from the pane, and keep the parse
	// independent of anything the shell might print ahead of it.
	deviceAuthNoSession   = "KYBER_DEVICE_AUTH_NO_SESSION"
	deviceAuthStartPrefix = "KYBER_DEVICE_AUTH_START="
	deviceAuthPaneMarker  = "KYBER_DEVICE_AUTH_PANE"

	// The exec delivers a capture and exits. Anything near this bound means
	// the exec is wedged, not that the flow is slow.
	deviceAuthExecTimeout = 15 * time.Second
)

// deviceAuthProbeScript reads both halves of the answer in one exec: when the
// login session started (which anchors the expiry countdown, since the flow
// only ever states a relative window) and what its pane currently shows.
//
// It always exits 0. A missing session is a normal state — the flow has not
// started, or it has finished and torn down — and turning that into a non-zero
// exit would make the handler guess at the difference between "no session" and
// "the exec itself failed".
const deviceAuthProbeScript = `
tmux has-session -t auth 2>/dev/null || { echo ` + deviceAuthNoSession + `; exit 0; }
echo "` + deviceAuthStartPrefix + `$(tmux display-message -p -t auth '#{session_created}' 2>/dev/null)"
echo ` + deviceAuthPaneMarker + `
tmux capture-pane -pJ -t auth 2>/dev/null || true
`

func (s *Server) handleCodexDeviceAuthStatus(w http.ResponseWriter, r *http.Request, name string) {
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}
	// Same guard and wording as the POST — asking a Claude Code agent about a
	// Codex device login is a caller bug, not an empty result.
	if agent.Spec.Runtime != "codex" || agent.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeOAuth {
		writeJSONErrorWithField(w, http.StatusConflict, "invalid_auth_mode",
			"device auth is available only for Codex agents using a ChatGPT subscription", "secrets.authType")
		return
	}

	if s.RestConfig == nil || s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"codex device auth status not available: restConfig/clientset not configured")
		return
	}

	// No live pod means the flow cannot be running yet. That is the first
	// ~20s of every click, so it reads as `starting` rather than an error —
	// the panel would otherwise flash a failure on the happy path.
	podName := "agent-" + name
	pod := &corev1.Pod{}
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Name: podName, Namespace: s.Namespace}, pod); err != nil {
		writeJSON(w, http.StatusOK, codexauth.Result{State: codexauth.StateStarting})
		return
	}
	if pod.Status.Phase != corev1.PodRunning {
		writeJSON(w, http.StatusOK, codexauth.Result{State: codexauth.StateStarting})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), deviceAuthExecTimeout)
	defer cancel()

	argv := append(deviceAuthNsenterPrefix(), deviceAuthProbeArgv()...)
	stdout, stderr, err := s.execRestartSession(ctx, podName, argv)
	if err != nil {
		// Two very different failures used to land here identically, and
		// collapsing them is what hid a broken probe for a whole release: the
		// argv was malformed, every poll came back with a shell syntax error,
		// and the panel spun over a code that was sitting in the pane.
		//
		// A probe that COULD NOT RUN will not fix itself, so say so. A probe
		// that ran and failed for a transient reason — the pod disappearing
		// between the Get above and the exec, which is exactly what the
		// operator just asked for — stays `starting` so the panel holds its
		// spinner instead of flashing a failure on the happy path.
		if probeCouldNotRun(stderr) {
			slog.Error("codex device auth probe could not run",
				"agent", name, "error", err, "stderr", firstLine(stderr))
			writeJSON(w, http.StatusOK, codexauth.Result{
				State:  codexauth.StateFailed,
				Detail: truncateForOperator(stderr),
			})
			return
		}
		slog.Warn("codex device auth probe failed",
			"agent", name, "error", err, "stderr", firstLine(stderr))
		writeJSON(w, http.StatusOK, codexauth.Result{State: codexauth.StateStarting})
		return
	}

	writeJSON(w, http.StatusOK, parseDeviceAuthProbe(stdout, time.Now()))
}

// parseDeviceAuthProbe turns the probe script's stdout into a Result. Split out
// from the handler so the marker handling is unit-testable without a cluster.
func parseDeviceAuthProbe(stdout string, now time.Time) codexauth.Result {
	if strings.Contains(stdout, deviceAuthNoSession) {
		return codexauth.Result{State: codexauth.StateAbsent}
	}

	var startedAt time.Time
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, deviceAuthStartPrefix) {
			continue
		}
		if secs, err := strconv.ParseInt(strings.TrimPrefix(line, deviceAuthStartPrefix), 10, 64); err == nil && secs > 0 {
			startedAt = time.Unix(secs, 0)
		}
		break
	}

	// Everything before the marker is the shell banner and our own echoes.
	// Feeding it to the parser would be harmless but pointless; cutting it
	// keeps the pane the parser sees the same as the pane the operator sees.
	pane := stdout
	if _, after, found := strings.Cut(stdout, deviceAuthPaneMarker); found {
		pane = after
	}
	return codexauth.Parse(pane, startedAt, now)
}

// deviceAuthProbeArgv becomes the agent user and runs the probe script.
//
// runuser, NOT `sudo -iu kyber`. `sudo -i` starts a LOGIN shell and re-parses
// the command it is handed, which mangles any real shell syntax in it: on
// kyber-canary this script came back as
//
//	bash: -c: line 2: syntax error: unexpected end of file from `{' command on line 1
//
// every single time, so the handler logged a warning and answered `starting`
// forever while a perfectly good code sat in the pane. Every other place the
// platform runs something as the agent user already uses runuser
// (pkg/runtimes/claudecode/adapter.go, pkg/runtimes/codex/adapter.go); the
// `sudo -iu` form belongs to the exec route's interactive modes, which pass
// plain argv with no shell syntax to mangle.
//
// Without -l there is no login profile, so the pod's debug banner does not
// appear either. The markers stay regardless — they are what makes the parse
// independent of whatever the shell decides to print.
//
// tmux resolves its socket per-uid, so this must land as `kyber` and not root,
// or it reports no session against a healthy pod.
func deviceAuthProbeArgv() []string {
	return []string{"/usr/sbin/runuser", "-u", "kyber", "--", "bash", "-c", deviceAuthProbeScript}
}

// deviceAuthNsenterPrefix mirrors the exec route's prefix: the tmux socket
// lives inside PID 1's remounted root, so a bare exec looks at the wrong
// filesystem and reports no session on a perfectly healthy pod.
func deviceAuthNsenterPrefix() []string {
	return []string{
		"nsenter", "--target", "1",
		"--mount", "--uts", "--ipc", "--net", "--pid",
		"--root", "--wd",
		"--",
	}
}

// probeCouldNotRun reports whether the failure was the probe never running, as
// opposed to running and failing.
//
// Two shapes matter, and both are permanent until somebody ships a fix:
//
//   - A launcher saying it could not start what we handed it —
//     `runuser: failed to execute …`, `nsenter: …: No such file or directory`.
//     The agent image is missing something the control plane assumes.
//   - The shell rejecting our own script — `bash: -c: line 2: syntax error: …`.
//     This is the exact stderr kyber-canary returned on every poll before the
//     runuser fix, and the case this classifier exists to stop hiding.
//
// Everything else is treated as transient. Deliberately conservative: a
// mis-classified transient failure turns a spinner into a scary message on the
// happy path, which is worse than one extra poll.
//
// Kept local rather than reusing isMissingInPodScript, which is anchored to the
// compact-session script's own name and would need widening to a second caller
// to be useful here.
func probeCouldNotRun(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		l := strings.ToLower(strings.TrimSpace(strings.Trim(line, `"`)))
		if l == "" {
			continue
		}
		if !hasAnyPrefix(l, "runuser:", "nsenter:", "bash:", "sh:", "su:", "sudo:") {
			continue
		}
		// Our own script being rejected by the shell that was handed it.
		if strings.Contains(l, "syntax error") || strings.Contains(l, "unexpected end of file") {
			return true
		}
		// A launcher that could not start the next thing in the chain.
		if strings.Contains(l, "failed to execute") ||
			strings.Contains(l, "no such file or directory") ||
			strings.Contains(l, "command not found") ||
			strings.Contains(l, "permission denied") {
			return true
		}
	}
	return false
}

// truncateForOperator bounds the stderr echoed into the API response. This is
// stderr, never stdout — the pane, and therefore the code, only ever arrives on
// stdout, so nothing secret rides along here.
func truncateForOperator(stderr string) string {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(stderr), `"`))
	if s == "" {
		return "Kyber could not read the login session from the agent."
	}
	const max = 300
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// firstLine keeps a stderr sample out of the multi-kilobyte range in logs.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return fmt.Sprintf("%q", s)
}
