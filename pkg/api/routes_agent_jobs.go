package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

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
	found := false
	for _, j := range agent.Spec.Jobs {
		if j.Name == jobName {
			found = true
			break
		}
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"agent '"+agentName+"' has no job named '"+jobName+"'")
		return
	}

	if s.RestConfig == nil || s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"run not available: restConfig not configured")
		return
	}

	// Exec the same dispatcher the crontab line would. Keep the exec short —
	// the dispatcher is fire-and-forget: it sends to tmux, POSTs its event,
	// and exits. 30s is comfortable headroom.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	stdout, stderr, err := s.execRunJob(ctx, "agent-"+agentName, jobName)
	if err != nil {
		slog.Error("run-job exec failed", "agent", agentName, "job", jobName, "error", err, "stderr", stderr)
		writeJSONError(w, http.StatusInternalServerError, "exec_failed",
			fmt.Sprintf("exec failed: %v", err))
		return
	}

	// The dispatcher always exits 0 even on failed dispatch — outcome is in
	// status.jobs[]. Return the captured output so the PWA can surface any
	// unexpected stderr in a toast.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"agent":  agentName,
		"job":    jobName,
		"stdout": stdout,
		"stderr": stderr,
	})
}

// execRunJob runs `kyber-job-dispatch <jobName>` inside the agent pod and
// returns its stdout/stderr. Non-interactive: no stdin, no TTY. Uses the
// same SPDY pathway as the websocket exec proxy.
//
// kubectl exec lands in the pod's outer namespaces, but the agent's tmux
// session and Claude Code process live inside PID 1's chroot (the
// whole-disk-persistence overlay) running as the kyber user. Without
// nsenter+user-switch, `tmux has-session` returns "no such session" and
// every dispatch fails with tmux_session_absent. The nsenter prefix
// mirrors routes_exec.go's shell-tab pattern (kyber#125 D2 — full
// namespace set).
//
// Uses `runuser -u kyber --` (not `sudo -iu kyber`) so AGENT_NAME +
// KYBER_CONTROL_PLANE_INTERNAL_URL et al. survive the user switch — the
// in-pod kyber-job-dispatch script POSTs back to the control plane to
// record outcome events, and that POST is silently dropped when those
// env vars are stripped. Caught during the kyber#208 Phase 3 manual e2e:
// the prior hotfix (`sudo -iu`) made `tmux has-session` succeed but
// emit_event no-op'd because $CONTROL_PLANE_URL was empty.
func (s *Server) execRunJob(ctx context.Context, podName, jobName string) (string, string, error) {
	execReq := s.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(s.Namespace).
		SubResource("exec").
		Param("container", "agent").
		Param("stdout", "true").
		Param("stderr", "true").
		Param("command", "nsenter").
		Param("command", "--target").
		Param("command", "1").
		Param("command", "--mount").
		Param("command", "--uts").
		Param("command", "--ipc").
		Param("command", "--net").
		Param("command", "--pid").
		Param("command", "--root").
		Param("command", "--wd").
		Param("command", "--").
		Param("command", "runuser").
		Param("command", "-u").
		Param("command", "kyber").
		Param("command", "--").
		Param("command", "/usr/local/bin/kyber-job-dispatch").
		Param("command", jobName)

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

// ExecRunJobStdin is the exported alias for execRunJobStdin — main.go's
// inbound queue handler closure needs to call this from another package.
func (s *Server) ExecRunJobStdin(ctx context.Context, podName, jobName string, stdin io.Reader) (string, string, error) {
	return s.execRunJobStdin(ctx, podName, jobName, stdin)
}

// execRunJobStdin runs `kyber-job-dispatch --stdin <jobName>` inside the agent
// pod with the rendered prompt fed on stdin. Used by the inbound-prompts
// receiver — same SPDY pathway as execRunJob, but with stdin wired so the
// in-pod helper reads the prompt body from the stream instead of from the
// per-job ConfigMap-projected file. <jobName> is "inbound-<requestID>" so
// the in-pod log/event lines remain tied back to a specific HTTP request.
func (s *Server) execRunJobStdin(ctx context.Context, podName, jobName string, stdin io.Reader) (string, string, error) {
	// Same nsenter+sudo prefix as execRunJob — see that function's comment.
	// The receiver was caught in production exec'ing the script outside the
	// chroot, which made tmux_session_absent the universal outcome for
	// every inbound prompt. sudo -iu kyber preserves stdin so the --stdin
	// flag still receives the rendered envelope.
	execReq := s.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(s.Namespace).
		SubResource("exec").
		Param("container", "agent").
		Param("stdin", "true").
		Param("stdout", "true").
		Param("stderr", "true").
		Param("command", "nsenter").
		Param("command", "--target").
		Param("command", "1").
		Param("command", "--mount").
		Param("command", "--uts").
		Param("command", "--ipc").
		Param("command", "--net").
		Param("command", "--pid").
		Param("command", "--root").
		Param("command", "--wd").
		Param("command", "--").
		Param("command", "runuser").
		Param("command", "-u").
		Param("command", "kyber").
		Param("command", "--").
		Param("command", "/usr/local/bin/kyber-job-dispatch").
		Param("command", "--stdin").
		Param("command", jobName)

	executor, err := remotecommand.NewSPDYExecutor(s.RestConfig, "POST", execReq.URL())
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

// ensure json is referenced (future-proofing if we start accepting bodies)
var _ = json.Marshal
