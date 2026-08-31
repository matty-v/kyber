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
