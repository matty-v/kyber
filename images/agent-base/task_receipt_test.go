package agent_base_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskReceiptHookPersistsAndProvesReceipt(t *testing.T) {
	var stored map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
				t.Error(err)
				http.Error(w, "bad", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": stored})
			return
		}
		if stored == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(stored)
	}))
	defer server.Close()
	payload := `{"prompt":"[kyber-task:task_11111111111111111111111111111111] attempt=attempt_22222222222222222222222222222222\nwork","session_id":"session-1","turn_id":"turn-1","hook_event_name":"UserPromptSubmit"}`
	cmd := exec.Command("bash", "scripts/kyber-task-receipt", "codex")
	cmd.Stdin = strings.NewReader(payload)
	dir := t.TempDir()
	cmd.Env = append(os.Environ(), "KYBER_TASK_RECEIPT_URL="+server.URL, "KYBER_TASK_RECEIPT_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook: %v: %s", err, out)
	}
	if stored["taskId"] != "task_11111111111111111111111111111111" || stored["attemptId"] != "attempt_22222222222222222222222222222222" || stored["runtime"] != "codex" {
		t.Fatalf("receipt=%v", stored)
	}
	if _, err := os.Stat(filepath.Join(dir, "attempt_22222222222222222222222222222222.json")); err != nil {
		t.Fatal(err)
	}
}

func TestTaskReceiptHookOmitsEmptyTurnID(t *testing.T) {
	var stored map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"receipt": stored})
	}))
	defer server.Close()
	payload := `{"prompt":"[kyber-task:task_11111111111111111111111111111111] attempt=attempt_22222222222222222222222222222222\nwork","session_id":"session-1","hook_event_name":"UserPromptSubmit"}`
	cmd := exec.Command("bash", "scripts/kyber-task-receipt", "claude-code")
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = append(os.Environ(), "KYBER_TASK_RECEIPT_URL="+server.URL, "KYBER_TASK_RECEIPT_DIR="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook: %v: %s", err, out)
	}
	if _, ok := stored["turnId"]; ok {
		t.Fatalf("empty turnId must use the server's canonical omitted form: %v", stored)
	}
}

// receiptServer records every receipt the hook POSTs and answers like the
// sidecar does.
func receiptServer(t *testing.T) (*httptest.Server, *[]map[string]string) {
	t.Helper()
	var got []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		defer r.Body.Close()
		var receipt map[string]string
		if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		got = append(got, receipt)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt})
	}))
	t.Cleanup(server.Close)
	return server, &got
}

func runReceiptHook(t *testing.T, serverURL, prompt string) (logPath string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"prompt": prompt, "session_id": "session-1", "hook_event_name": "UserPromptSubmit"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "receipt.log")
	cmd := exec.Command("bash", "scripts/kyber-task-receipt", "claude-code")
	cmd.Stdin = strings.NewReader(string(payload))
	cmd.Env = append(os.Environ(), "KYBER_TASK_RECEIPT_URL="+serverURL, "KYBER_TASK_RECEIPT_DIR="+filepath.Join(dir, "receipts"), "KYBER_TASK_RECEIPT_LOG="+logPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook must not fail here: %v: %s", err, out)
	}
	return logPath
}

const testEnvelope = "[kyber-task:task_11111111111111111111111111111111] attempt=attempt_22222222222222222222222222222222\nagent: jack\ntask:\n  id: task_11111111111111111111111111111111\n  attempt_id: attempt_22222222222222222222222222222222"

// The 2026-10-06 regression: Claude Code wraps a multi-line bracketed paste in
// <pasted_content id=…>, so the header was no longer the first line, the hook
// exited 0 without a receipt, and every durable task's complete was rejected.
func TestTaskReceiptHookAcceptsClaudeCodePasteWrapper(t *testing.T) {
	server, got := receiptServer(t)
	prompt := "\n\n<pasted_content id=\"2b5a\">\n" + testEnvelope + "\n</pasted_content id=\"2b5a\">\n"
	runReceiptHook(t, server.URL, prompt)
	if len(*got) != 1 || (*got)[0]["taskId"] != "task_11111111111111111111111111111111" || (*got)[0]["attemptId"] != "attempt_22222222222222222222222222222222" {
		t.Fatalf("want one receipt for the wrapped envelope, got %v", *got)
	}
}

// A header quoted inside someone's message must never bind a receipt, and the
// near-miss is logged instead of passing silently.
func TestTaskReceiptHookIgnoresQuotedHeaderAndLogsIt(t *testing.T) {
	cases := map[string]string{
		"quoted mid-message":     "here is the log you asked for:\n" + testEnvelope,
		"quoted inside wrapper":  "\n\n<pasted_content id=\"2b5a\">\nlog follows\n" + testEnvelope + "\n</pasted_content id=\"2b5a\">\n",
		"unknown wrapper":        "<pasted_text id=\"2b5a\">\n" + testEnvelope + "\n</pasted_text id=\"2b5a\">",
		"wrapper with no closer": "\n\n<pasted_content id=\"2b5a\">\n" + testEnvelope,
	}
	for name, prompt := range cases {
		t.Run(name, func(t *testing.T) {
			server, got := receiptServer(t)
			logPath := runReceiptHook(t, server.URL, prompt)
			if len(*got) != 0 {
				t.Fatalf("must not post a receipt, got %v", *got)
			}
			b, err := os.ReadFile(logPath)
			if err != nil || !strings.Contains(string(b), "event=header_not_first_line") {
				t.Fatalf("want a header_not_first_line log line, got %q (err=%v)", b, err)
			}
		})
	}
}

// Ordinary prompts neither post a receipt nor write to the log.
func TestTaskReceiptHookStaysQuietForOrdinaryPrompts(t *testing.T) {
	server, got := receiptServer(t)
	logPath := runReceiptHook(t, server.URL, "\n\n<pasted_content id=\"2b5a\">\nhey, what's the status\nof the deploy?\n</pasted_content id=\"2b5a\">\n")
	if len(*got) != 0 {
		t.Fatalf("must not post a receipt, got %v", *got)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("must not log for an ordinary prompt (err=%v)", err)
	}
}
