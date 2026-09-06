package agent_base_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// HC-07: run the production receipt hook against a controllable sidecar.
// This verifies transport evidence, not whether an upstream CLI honors exit 2.
func TestHarnessContractReceiptRecovery(t *testing.T) {
	for _, runtime := range []string{"claude-code", "codex"} {
		for _, scenario := range []string{"accepted", "response-lost", "unavailable", "mismatched", "ordinary", "wrong-event", "missing-session"} {
			t.Run(runtime+"/"+scenario, func(t *testing.T) {
				task := "task_11111111111111111111111111111111"
				attempt := "attempt_22222222222222222222222222222222"
				want := map[string]string{"taskId": task, "attemptId": attempt, "runtime": runtime, "sessionId": "session-1"}
				input := map[string]string{"prompt": "[kyber-task:" + task + "] attempt=" + attempt + "\nPRIVATE_PROMPT_SENTINEL", "session_id": "session-1", "hook_event_name": "UserPromptSubmit"}
				if runtime == "codex" {
					input["turn_id"] = "turn-1"
					want["turnId"] = "turn-1"
				}
				switch scenario {
				case "ordinary":
					input["prompt"] = "ordinary PRIVATE_PROMPT_SENTINEL"
				case "wrong-event":
					input["hook_event_name"] = "Stop"
				case "missing-session":
					delete(input, "session_id")
				}
				var posts, gets atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						posts.Add(1)
						var got map[string]string
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
							t.Error(err)
						}
						if len(got) != len(want) {
							t.Errorf("receipt includes unexpected fields: %v", got)
						}
						for k, v := range want {
							if got[k] != v {
								t.Errorf("receipt field %s differs", k)
							}
						}
						if scenario == "response-lost" {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							_ = conn.Close()
							return
						}
					} else {
						gets.Add(1)
						if r.Method != http.MethodGet || r.URL.Path != "/"+attempt {
							t.Errorf("unexpected recovery request: %s %s", r.Method, r.URL.Path)
						}
					}
					if scenario == "unavailable" {
						http.Error(w, "unavailable", 503)
						return
					}
					body := map[string]string{}
					for k, v := range want {
						body[k] = v
					}
					if scenario == "mismatched" {
						body["attemptId"] = "attempt_33333333333333333333333333333333"
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				data, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				run := func() ([]byte, error) {
					cmd := exec.Command("bash", "scripts/kyber-task-receipt", runtime)
					cmd.Stdin = strings.NewReader(string(data))
					cmd.Env = append(os.Environ(), "KYBER_TASK_RECEIPT_URL="+server.URL, "KYBER_TASK_RECEIPT_DIR="+dir)
					return cmd.CombinedOutput()
				}
				out, err := run()
				code := 0
				if err != nil {
					if e, ok := err.(*exec.ExitError); ok {
						code = e.ExitCode()
					} else {
						t.Fatal(err)
					}
				}
				expected := 0
				if scenario == "unavailable" || scenario == "mismatched" || scenario == "wrong-event" || scenario == "missing-session" {
					expected = 2
				}
				if code != expected {
					t.Fatalf("exit %d, want %d: %s", code, expected, out)
				}
				if strings.Contains(string(out), "PRIVATE_PROMPT_SENTINEL") {
					t.Fatal("prompt leaked to hook output")
				}
				if scenario == "ordinary" || scenario == "wrong-event" || scenario == "missing-session" {
					if posts.Load() != 0 || gets.Load() != 0 {
						t.Fatal("non-acceptance input contacted sidecar")
					}
					entries, err := os.ReadDir(dir)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) != 0 {
						t.Fatal("non-acceptance input created receipt")
					}
					return
				}
				if posts.Load() != 1 {
					t.Errorf("POST count=%d", posts.Load())
				}
				expectedGets := int32(0)
				if scenario != "accepted" {
					expectedGets = 1
				}
				if gets.Load() != expectedGets {
					t.Errorf("GET count=%d, want %d", gets.Load(), expectedGets)
				}
				receipt, err := os.ReadFile(filepath.Join(dir, attempt+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(receipt), "PRIVATE_PROMPT_SENTINEL") {
					t.Fatal("prompt persisted in receipt")
				}
				info, err := os.Stat(filepath.Join(dir, attempt+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Errorf("receipt permissions=%o", info.Mode().Perm())
				}
				// A reused local attempt with conflicting session evidence must fail
				// before calling the sidecar, including after an uncertain delivery.
				input["session_id"] = "different-session"
				data, err = json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				out, err = run()
				if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 2 {
					t.Fatalf("conflict must fail closed: %v %s", err, out)
				}
				if posts.Load() != 1 || gets.Load() != expectedGets {
					t.Fatal("conflicting local receipt contacted sidecar")
				}
			})
		}
	}
}
