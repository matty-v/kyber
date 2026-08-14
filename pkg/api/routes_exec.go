package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// execControlMsg is a JSON text frame sent by the client to control the exec session.
// Currently only "resize" is supported.
type execControlMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// handleAgentExec proxies a WebSocket exec session into the agent's pod.
//
// GET /api/v1/agents/{name}/exec (Upgrade: websocket)
//
// GET, not POST. The route dispatch in handleAgents does not gate on method,
// so POST reaches this handler — but gorilla's Upgrade() rejects any non-GET
// request with a plain-text 405 before a session is ever established, and RFC
// 6455 gives a browser no way to open a WebSocket with anything but GET.
//
// Query parameters:
//
//	cmd  — command to run inside the pod. May appear multiple times to specify
//	       argv. If absent, falls back to mode (below) or "/bin/bash".
//	mode — convenience alias. "attach" → read-only tmux attach to the agent.
//	       "shell" → root login bash. "history" → one-shot capture of the
//	       agent's tmux pane + scrollback (for conversation review; ws closes
//	       after the capture is streamed). Ignored when cmd is present.
//	       "device-auth" → read-only attach to Codex's device-login session.
//	       Unknown values cause a 400.
//
// WebSocket protocol:
//
//	Client → server: binary frames are stdin bytes; text frames are JSON control msgs.
//	Server → client: binary frames are stdout/stderr bytes interleaved.
func (s *Server) handleAgentExec(w http.ResponseWriter, r *http.Request, name string) {
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent for exec", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	if !validateMode(r) {
		writeJSONError(w, http.StatusBadRequest, "invalid_mode",
			"unknown mode; want 'attach', 'shell', 'history', or 'device-auth'")
		return
	}

	// Pod name is deterministic: "agent-<name>".
	// Kept inline to avoid pulling pkg/controllers/agent into the API layer.
	podName := "agent-" + name

	if s.RestConfig == nil || s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"exec not available: restConfig not configured")
		return
	}

	// Agent pods have multiple containers since kyber#248 (runtime + status
	// sidecar). The k8s exec API requires `container=<name>` to disambiguate;
	// without it kubelet picks an unspecified container — the sidecar is
	// distroless, so the connection appears to succeed but nothing flows.
	// Always target the runtime container by name.
	s.execIntoPod(w, r, podName, "agent")
}

// handleMachineExec proxies a WebSocket exec session into the node-agent pod for a Machine.
//
// GET /api/v1/machines/{name}/exec (Upgrade: websocket) — see handleAgentExec
// for why this is GET and not POST.
func (s *Server) handleMachineExec(w http.ResponseWriter, r *http.Request, name string) {
	machine := &kyberv1.Machine{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, machine); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "machine '"+name+"' not found")
			return
		}
		slog.Error("failed to get machine for exec", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get machine")
		return
	}

	if !validateMode(r) {
		writeJSONError(w, http.StatusBadRequest, "invalid_mode",
			"unknown mode; want 'attach', 'shell', or 'history'")
		return
	}

	if s.RestConfig == nil || s.Clientset == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"exec not available: restConfig not configured")
		return
	}

	// Find node-agent pod.
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

	// node-agent DaemonSet pod has a single container named "node-agent"
	// (deploy/helm/kyber/templates/node-agent/daemonset.yaml). Pass it
	// explicitly so the exec path is single-container-agnostic.
	s.execIntoPod(w, r, podList.Items[0].Name, "node-agent")
}

// execStreamParams returns the standard query-param set for the k8s exec
// API. Pulled out as a pure function so a unit test can pin the
// container-name regression from kyber#264 — pre-fix the Param("container")
// line was missing, the connection landed in an unspecified container of
// the multi-container agent pod (post-kyber#248), and Activity + Shell
// silently broke for any agent.
//
// Every key here is required by the k8s API. `container` is the
// regression we care about; the rest are stable.
func execStreamParams(container string) map[string]string {
	return map[string]string{
		"container": container,
		"stdin":     "true",
		"stdout":    "true",
		"stderr":    "true",
		"tty":       "true",
	}
}

// execIntoPod is the shared exec implementation used by both agent and machine exec handlers.
func (s *Server) execIntoPod(w http.ResponseWriter, r *http.Request, podName, container string) {
	cmd := parseExecCommand(r)

	execReq := s.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(s.Namespace).
		SubResource("exec")
	for k, v := range execStreamParams(container) {
		execReq = execReq.Param(k, v)
	}
	for _, c := range cmd {
		execReq = execReq.Param("command", c)
	}

	executor, err := remotecommand.NewSPDYExecutor(s.RestConfig, "POST", execReq.URL())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error",
			"failed to create executor: "+err.Error())
		return
	}

	conn := upgradeWebSocket(w, r)
	if conn == nil {
		return
	}
	defer conn.Close()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	sizeCh := make(chan remotecommand.TerminalSize, 4)

	// Goroutine: WebSocket reads → exec stdin + resize signals.
	wsReadDone := make(chan struct{})
	go func() {
		defer close(wsReadDone)
		defer stdinW.Close()
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch msgType {
			case websocket.BinaryMessage:
				if _, err := stdinW.Write(data); err != nil {
					return
				}
			case websocket.TextMessage:
				var ctrl execControlMsg
				if err := json.Unmarshal(data, &ctrl); err != nil {
					continue
				}
				if ctrl.Type == "resize" && ctrl.Cols > 0 && ctrl.Rows > 0 {
					select {
					case sizeCh <- remotecommand.TerminalSize{Width: ctrl.Cols, Height: ctrl.Rows}:
					default:
					}
				}
			}
		}
	}()

	// Goroutine: exec stdout → WebSocket binary frames.
	execReadDone := make(chan struct{})
	go func() {
		defer close(execReadDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := stdoutR.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Run the exec session (blocks until the command exits or streams close).
	streamErr := executor.StreamWithContext(r.Context(), remotecommand.StreamOptions{
		Stdin:             stdinR,
		Stdout:            stdoutW,
		Stderr:            stdoutW, // interleave stderr into the same binary stream
		Tty:               true,
		TerminalSizeQueue: &terminalSizeQueue{ch: sizeCh},
	})
	stdoutW.Close()
	stdinR.Close()

	// Wait for I/O goroutines to finish.
	<-wsReadDone
	<-execReadDone

	// Send a close frame, optionally carrying the exit error.
	if streamErr != nil {
		payload, err := encodeExecExitPayload(streamErr.Error())
		if err == nil {
			_ = conn.WriteMessage(websocket.TextMessage, payload)
		}
	} else {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"exit"}`))
	}
}

func encodeExecExitPayload(message string) ([]byte, error) {
	return json.Marshal(struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}{Type: "exit", Error: message})
}

// terminalSizeQueue implements remotecommand.TerminalSizeQueue.
type terminalSizeQueue struct {
	ch chan remotecommand.TerminalSize
}

func (q *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return &size
}

// parseExecCommand parses the ?cmd=... and ?mode=... query parameters.
//
// Precedence: ?cmd= (if present) > ?mode= (if recognized) > default ["/bin/bash"].
//
// ?mode= shortcuts:
//
//	attach  — read-only attach to the agent's tmux session. Still exposed
//	          for direct API/curl debugging; the PWA Activity tab no longer
//	          surfaces this mode (2026-04-21) because `history` is a
//	          strictly better UX for observing Claude's conversation.
//	shell   — root login bash with the kyber MOTD + aliases sourced
//	history — one-shot `tmux capture-pane -peJ -S -10000 -t agent`: prints
//	          the current pane plus as much scrollback as tmux's history-limit
//	          holds, then exits. Used by the PWA "History" tab so operators
//	          can wheel-scroll conversation content natively in xterm's
//	          scrollback (PR #122 V2 — replaces the wheel→PgUp approach that
//	          didn't work because Claude Code doesn't bind those keys).
//
// Both modes wrap the command in nsenter targeting PID 1 with the full
// namespace set (-m mount, -u UTS, -i IPC, -n net, -p PID, -r root, -w wd).
// In overlay mode the agent runs inside /merged via chroot, so the agent's
// tmux socket and the in-chroot filesystem / process list are only visible
// from PID 1's root view; a plain kubectl exec lands in the container's
// original rootfs and can't see them. The full namespace set is D2 from
// issue #125 — earlier iterations used only mount+root, which was enough
// for filesystem reads but left `ps` / network tooling pointing at the
// pod's original namespaces.
//
// If cmd appears multiple times the values are the argv (e.g. /bin/sh -c ls).
// If only one value is provided and contains spaces it is split on whitespace.
func parseExecCommand(r *http.Request) []string {
	if vals := r.URL.Query()["cmd"]; len(vals) > 0 {
		if len(vals) == 1 {
			parts := strings.Fields(vals[0])
			if len(parts) > 0 {
				return parts
			}
			return []string{"/bin/bash"}
		}
		return vals
	}
	nsenterPrefix := []string{
		"nsenter", "--target", "1",
		"--mount", "--uts", "--ipc", "--net", "--pid",
		"--root", "--wd",
		"--",
	}
	switch r.URL.Query().Get("mode") {
	case "attach":
		return append(nsenterPrefix, "sudo", "-iu", "kyber", "tmux", "attach", "-t", "agent", "-r")
	case "shell":
		return append(nsenterPrefix, "/bin/bash", "-l")
	case "history":
		// -p prints to stdout; -e preserves escapes (colors, box-drawing);
		// -J joins wrapped lines; -S -10000 grabs up to 10k scrollback lines.
		// The command exits when the capture is complete; the websocket
		// layer closes on exit, which the client treats as end-of-stream.
		return append(nsenterPrefix, "sudo", "-iu", "kyber", "tmux", "capture-pane", "-peJ", "-S", "-10000", "-t", "agent")
	case "device-auth":
		return append(nsenterPrefix, "sudo", "-iu", "kyber", "tmux", "attach", "-t", "auth", "-r")
	}
	return []string{"/bin/bash"}
}

// ParseExecCommandForTest exposes parseExecCommand for tests in the api_test package.
func ParseExecCommandForTest(r *http.Request) []string {
	return parseExecCommand(r)
}

// validateMode returns false for any non-empty mode that isn't recognized.
func validateMode(r *http.Request) bool {
	m := r.URL.Query().Get("mode")
	return m == "" || m == "attach" || m == "shell" || m == "history" || m == "device-auth"
}
