package api

import (
	"bytes"
	"context"
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
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Exit statuses kyber-job-dispatch uses in --run-now mode.
const (
	runNowExitFailed  = 3
	runNowExitSkipped = 5
)

var runNowReasonRe = regexp.MustCompile(`outcome=\S+ reason=(\S+)`)

// handleAgentJobAction dispatches sub-paths under /api/v1/agents/{name}/jobs/.
// Currently the only supported sub-path is "{jobName}/run" (POST).
func (s *Server) handleAgentJobAction(w http.ResponseWriter, r *http.Request, agentName, subpath string) {
	parts := strings.Split(subpath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "run" {
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown jobs action")
		return
	}
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	jobName := parts[0]
	if !jobNameRe.MatchString(jobName) {
		writeJSONError(w, http.StatusBadRequest, "invalid_job_name", "invalid job name")
		return
	}

	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(),
		types.NamespacedName{Name: agentName, Namespace: s.Namespace}, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return
		}
		slog.Error("failed to get agent for job run", "name", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	// Job must be declared on spec.jobs — run-now is an operator convenience
	// for scheduled jobs, not an arbitrary command-execution backdoor.
	var job *kyberv1.AgentJob
	for i := range agent.Spec.Jobs {
		if agent.Spec.Jobs[i].Name == jobName {
			job = &agent.Spec.Jobs[i]
			break
		}
	}
	if job == nil {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"agent '"+agentName+"' has no job named '"+jobName+"'")
		return
	}
	if job.Paused {
		writeJSONError(w, http.StatusConflict, "job_paused",
			"job '"+jobName+"' is paused; resume it before running it")
		return
	}

	exec := s.jobDispatchExec
	if exec == nil {
		if s.RestConfig == nil || s.Clientset == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
				"run not available: restConfig not configured")
			return
		}
		exec = s.execJobDispatch
	}

	// The prompt comes from spec.jobs, the source of truth, rather than the
	// pod's projected prompt file, which lags a just-saved job by up to a
	// minute. The job's own flags apply exactly as on a scheduled fire.
	args := []string{"--run-now"}
	if job.Exclusive {
		args = append(args, "--exclusive")
	}
	if job.ClearContextAfter {
		args = append(args, "--clear-context-after")
	}
	args = append(args, jobName)

	// The dispatcher only sends to tmux and records the outcome; 30s is
	// comfortable headroom.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stdout, stderr, err := exec(ctx, "agent-"+agentName, args, strings.NewReader(job.Prompt))
	if isOldDispatcher(err, stderr) {
		// The agent's image predates --run-now: runtime images are pinned per
		// cluster, so the control plane routinely runs ahead of them. Its
		// dispatcher takes the prompt with --stdin, but cannot report a
		// failed delivery.
		args[0] = "--stdin"
		stdout, stderr, err = exec(ctx, "agent-"+agentName, args, strings.NewReader(job.Prompt))
	}
	if err != nil {
		var exitErr utilexec.ExitError
		if errors.As(err, &exitErr) {
			reason := "unknown"
			if m := runNowReasonRe.FindStringSubmatch(stdout); m != nil {
				reason = m[1]
			}
			switch exitErr.ExitStatus() {
			case runNowExitSkipped:
				writeJSONError(w, http.StatusConflict, "job_skipped",
					fmt.Sprintf("job %q was not started: %s", jobName, reason))
				return
			case runNowExitFailed:
				writeJSONError(w, http.StatusServiceUnavailable, "job_failed",
					fmt.Sprintf("job %q could not be delivered to the agent: %s", jobName, reason))
				return
			}
		}
		slog.Error("run-job exec failed", "agent", agentName, "job", jobName, "error", err, "stderr", stderr)
		writeJSONError(w, http.StatusInternalServerError, "exec_failed",
			fmt.Sprintf("exec failed: %v", err))
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"agent":  agentName,
		"job":    jobName,
		"stdout": stdout,
		"stderr": stderr,
	})
}

// isOldDispatcher reports the usage error an agent image from before
// --run-now answers with.
func isOldDispatcher(err error, stderr string) bool {
	var exitErr utilexec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitStatus() == 2 && strings.Contains(stderr, "unknown flag --run-now")
}

// ExecRunJobStdin is the exported alias for execRunJobStdin — main.go's
// inbound queue handler closure needs to call this from another package.
func (s *Server) ExecRunJobStdin(ctx context.Context, podName, jobName string, stdin io.Reader) (string, string, error) {
	return s.execRunJobStdin(ctx, podName, jobName, stdin)
}

// execRunJobStdin runs `kyber-job-dispatch --stdin <jobName>` inside the agent
// pod with the rendered prompt fed on stdin. Used by the inbound-prompts
// receiver: the in-pod helper reads the prompt body from the stream instead
// of from the per-job ConfigMap-projected file. <jobName> is
// "inbound-<requestID>" so the in-pod log/event lines remain tied back to a
// specific HTTP request.
func (s *Server) execRunJobStdin(ctx context.Context, podName, jobName string, stdin io.Reader) (string, string, error) {
	return s.execJobDispatch(ctx, podName, []string{"--stdin", jobName}, stdin)
}

// execJobDispatch runs kyber-job-dispatch with args inside the agent pod,
// feeding stdin. Uses the same SPDY pathway as the websocket exec proxy.
//
// kubectl exec lands in the pod's outer namespaces, but the agent's tmux
// session and Claude Code process live inside PID 1's chroot (the
// whole-disk-persistence root) running as the kyber user. Without
// nsenter+user-switch, `tmux has-session` returns "no such session" and
// every dispatch fails with tmux_session_absent. The nsenter prefix mirrors
// routes_exec.go's shell-tab pattern (kyber#125 D2 — full namespace set).
//
// Uses `runuser -u kyber --` (not `sudo -iu kyber`) so AGENT_NAME +
// KYBER_CONTROL_PLANE_INTERNAL_URL et al. survive the user switch — the
// in-pod kyber-job-dispatch script POSTs back to the control plane to
// record outcome events, and that POST is silently dropped when those env
// vars are stripped (kyber#208 Phase 3). runuser also preserves stdin.
func (s *Server) execJobDispatch(ctx context.Context, podName string, args []string, stdin io.Reader) (string, string, error) {
	req := s.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(s.Namespace).
		SubResource("exec").
		Param("container", "agent").
		Param("stdin", "true").
		Param("stdout", "true").
		Param("stderr", "true")
	for _, arg := range append([]string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd", "--",
		"runuser", "-u", "kyber", "--", "/usr/local/bin/kyber-job-dispatch"}, args...) {
		req = req.Param("command", arg)
	}

	executor, err := remotecommand.NewSPDYExecutor(s.RestConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("new executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	}); err != nil {
		return stdout.String(), stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}
