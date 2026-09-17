package agent_base_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestGoalStartHookNeverForwardsPromptAndInjectsRevision(t *testing.T) {
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"summary":"Working on a new request","source":"platform","acceptedAt":"2026-09-16T12:00:00Z","updatedAt":"2026-09-16T12:00:00Z"}`))
	}))
	defer server.Close()

	cmd := exec.Command("bash", "scripts/kyber-goal-start")
	cmd.Env = append(cmd.Environ(), "KYBER_GOAL_START_URL="+server.URL)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"PRIVATE_PROMPT_SENTINEL"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook: %v: %s", err, out)
	}
	if _, ok := body["prompt"]; ok || strings.Contains(string(out), "PRIVATE_PROMPT_SENTINEL") {
		t.Fatalf("prompt leaked: body=%v output=%s", body, out)
	}
	if len(body) != 0 || !strings.Contains(string(out), "set_goal") || !strings.Contains(string(out), "2026-09-16T12:00:00Z") {
		t.Fatalf("body=%v output=%s", body, out)
	}
}

func TestGoalStartHookFailureNeverBlocksPrompt(t *testing.T) {
	cmd := exec.Command("bash", "scripts/kyber-goal-start")
	cmd.Env = append(cmd.Environ(), "KYBER_GOAL_START_URL=http://127.0.0.1:1")
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"work"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook blocked prompt: %v: %s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("failed hook output=%q", out)
	}
}
