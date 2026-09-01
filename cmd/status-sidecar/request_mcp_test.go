package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requestMCPCall(t *testing.T, handler http.HandlerFunc, method string, params any) requestRPCResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)))
	var response requestRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode MCP response: %v (%s)", err, recorder.Body.String())
	}
	return response
}

func TestRequestMCPInitializeAndToolList(t *testing.T) {
	server := &requestMCPServer{}
	initialized := requestMCPCall(t, server.handle, "initialize", map[string]any{})
	if initialized.Error != nil {
		t.Fatalf("initialize error = %+v", initialized.Error)
	}
	listed := requestMCPCall(t, server.handle, "tools/list", map[string]any{})
	encoded, _ := json.Marshal(listed.Result)
	for _, want := range []string{`"name":"get_self_profile"`, `"name":"respond"`, `"name":"report_progress"`, `"name":"publish_text"`, `"name":"publish_json"`, `"name":"get_control"`, `"name":"ack_cancel"`, `"name":"complete"`, `"request_id"`, `"task_id"`, `"attempt_id"`, `"response"`} {
		if !bytes.Contains(encoded, []byte(want)) {
			t.Fatalf("tools/list missing %s: %s", want, encoded)
		}
	}
}

func TestRequestMCPCancellationControlAndAcknowledgment(t *testing.T) {
	var paths []string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "task-control") {
			_, _ = w.Write([]byte(`{"cancel_requested":true,"reason":"superseded","requested_at":"2026-09-01T00:00:00Z"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	server := &requestMCPServer{client: cp.Client(), cfg: config{AgentName: "alice", ControlPlaneURL: cp.URL}}
	ids := map[string]any{"task_id": "task_11111111111111111111111111111111", "attempt_id": "attempt_22222222222222222222222222222222"}
	control := requestMCPCall(t, server.handle, "tools/call", map[string]any{"name": "get_control", "arguments": ids})
	structured := control.Result.(map[string]any)["structuredContent"].(map[string]any)
	if structured["cancel_requested"] != true || structured["reason"] != "superseded" {
		t.Fatalf("control=%+v", structured)
	}
	ids["acknowledgment_id"] = "ack_33333333333333333333333333333333"
	ids["note"] = "stopped future work; prior effects may remain"
	ack := requestMCPCall(t, server.handle, "tools/call", map[string]any{"name": "ack_cancel", "arguments": ids})
	if ack.Result.(map[string]any)["isError"] == true {
		t.Fatalf("ack=%+v", ack.Result)
	}
	if got := strings.Join(paths, ","); got != "/internal/agents/alice/task-control,/internal/agents/alice/task-cancel-ack" {
		t.Fatalf("paths=%s", got)
	}
}

func TestRequestMCPProgressAndInlineResults(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	server := &requestMCPServer{client: cp.Client(), cfg: config{AgentName: "alice", ControlPlaneURL: cp.URL}}
	calls := []map[string]any{
		{"name": "report_progress", "arguments": map[string]any{"task_id": "task_11111111111111111111111111111111", "attempt_id": "attempt_22222222222222222222222222222222", "update_id": "update_33333333333333333333333333333333", "message": "halfway", "percent": 50}},
		{"name": "publish_text", "arguments": map[string]any{"task_id": "task_11111111111111111111111111111111", "attempt_id": "attempt_22222222222222222222222222222222", "result_id": "result_44444444444444444444444444444444", "name": "summary", "text": "done"}},
		{"name": "publish_json", "arguments": map[string]any{"task_id": "task_11111111111111111111111111111111", "attempt_id": "attempt_22222222222222222222222222222222", "result_id": "result_55555555555555555555555555555555", "name": "manifest", "value": map[string]any{"pages": 8}}},
	}
	for _, call := range calls {
		response := requestMCPCall(t, server.handle, "tools/call", call)
		if result := response.Result.(map[string]any); result["isError"] == true {
			t.Fatalf("%s returned error: %+v", call["name"], result)
		}
	}
	if got := strings.Join(paths, ","); got != "/internal/agents/alice/task-progress,/internal/agents/alice/task-results,/internal/agents/alice/task-results" {
		t.Fatalf("paths = %s", got)
	}
	if bodies[0]["message"] != "halfway" || bodies[1]["kind"] != "text" || bodies[2]["kind"] != "json" {
		t.Fatalf("bodies = %+v", bodies)
	}
}

func TestRequestMCPPublishFileStreamsSafeResult(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "report.pdf"), []byte("%PDF-1.7\nfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var gotPath string
	var gotBody []byte
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		if r.Header.Get("X-Kyber-Task-Metadata") == "" {
			t.Fatal("missing task metadata")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"result":{"parts":[{"file":{"sha256":"f581fc87f30296eff11777c3ce1b9a8b7077071ad8abedfcba317fef0c807224"}}]}}`))
	}))
	defer cp.Close()
	server := &requestMCPServer{client: cp.Client(), cfg: config{AgentName: "alice", ControlPlaneURL: cp.URL, TaskResultsRoot: root}}
	response := requestMCPCall(t, server.handle, "tools/call", map[string]any{"name": "publish_file", "arguments": map[string]any{
		"task_id": "task_11111111111111111111111111111111", "attempt_id": "attempt_22222222222222222222222222222222",
		"result_id": "result_33333333333333333333333333333333", "name": "report", "path": filepath.Join(root, "report.pdf"),
	}})
	if result := response.Result.(map[string]any); result["isError"] == true {
		t.Fatalf("tool error: %+v", result)
	}
	if gotPath != "/internal/agents/alice/task-files" || string(gotBody) != "%PDF-1.7\nfixture" {
		t.Fatalf("path=%q body=%q", gotPath, gotBody)
	}
	response = requestMCPCall(t, server.handle, "tools/call", map[string]any{"name": "publish_file", "arguments": map[string]any{
		"task_id": "task_11111111111111111111111111111111", "attempt_id": "attempt_22222222222222222222222222222222",
		"result_id": "result_44444444444444444444444444444444", "name": "escape", "path": "/etc/passwd",
	}})
	if result := response.Result.(map[string]any); result["isError"] != true {
		t.Fatalf("outside path unexpectedly accepted: %+v", result)
	}
}

func TestRequestMCPGetSelfProfileReturnsStructuredContent(t *testing.T) {
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/agents/glyph/self-profile" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"glyph","runtime":"codex","resources":{"cpu":"2","memory":"8Gi","disk":"20Gi"},"skills":[]}`))
	}))
	defer controlPlane.Close()
	server := &requestMCPServer{client: controlPlane.Client(), cfg: config{AgentName: "glyph", ControlPlaneURL: controlPlane.URL}}
	response := requestMCPCall(t, server.handle, "tools/call", map[string]any{"name": "get_self_profile", "arguments": map[string]any{}})
	result := response.Result.(map[string]any)
	if result["isError"] == true {
		t.Fatalf("tool returned error: %+v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	if structured["name"] != "glyph" || structured["runtime"] != "codex" {
		t.Fatalf("structured content = %+v", structured)
	}
}

func TestRequestMCPCompleteForwardsTaskAndAttempt(t *testing.T) {
	var path string
	var payload map[string]string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	server := &requestMCPServer{client: cp.Client(), cfg: config{AgentName: "alice", ControlPlaneURL: cp.URL}}
	response := requestMCPCall(t, server.handle, "tools/call", map[string]any{"name": "complete", "arguments": map[string]any{"task_id": "task_11111111111111111111111111111111", "attempt_id": "attempt_22222222222222222222222222222222", "response": "done"}})
	result := response.Result.(map[string]any)
	if result["isError"] == true {
		t.Fatalf("tool error: %+v", result)
	}
	if path != "/internal/agents/alice/task-complete" || payload["response"] != "done" {
		t.Fatalf("path=%q payload=%v", path, payload)
	}
}

func TestRequestMCPRespondForwardsExactContent(t *testing.T) {
	var gotPath, gotAgent, gotResponse string
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		_ = json.Unmarshal(body, &payload)
		gotAgent = payload["request_id"]
		gotResponse = payload["response"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer controlPlane.Close()
	server := &requestMCPServer{
		client: controlPlane.Client(),
		cfg:    config{AgentName: "alice", ControlPlaneURL: controlPlane.URL},
	}
	response := requestMCPCall(t, server.handle, "tools/call", map[string]any{
		"name": "respond",
		"arguments": map[string]any{
			"request_id": "req_00000000000000000000000000000001",
			"response":   "  exact\nanswer  ",
		},
	})
	result := response.Result.(map[string]any)
	if result["isError"] == true {
		t.Fatalf("tool returned error: %+v", result)
	}
	if gotPath != "/internal/agents/alice/request-reply" || gotAgent != "req_00000000000000000000000000000001" || gotResponse != "  exact\nanswer  " {
		t.Fatalf("forwarded path=%q id=%q response=%q", gotPath, gotAgent, gotResponse)
	}
}

func TestRequestMCPRespondMapsStableErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "expired"},
		{http.StatusConflict, "different response"},
		{http.StatusRequestEntityTooLarge, "configured limit"},
		{http.StatusServiceUnavailable, "unavailable"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "sensitive upstream detail", tc.status)
			}))
			defer controlPlane.Close()
			server := &requestMCPServer{client: controlPlane.Client(), cfg: config{AgentName: "alice", ControlPlaneURL: controlPlane.URL}}
			response := requestMCPCall(t, server.handle, "tools/call", map[string]any{
				"name": "respond", "arguments": map[string]any{
					"request_id": "req_00000000000000000000000000000001", "response": "answer",
				},
			})
			encoded, _ := json.Marshal(response.Result)
			if !bytes.Contains(encoded, []byte(tc.want)) || bytes.Contains(encoded, []byte("sensitive")) {
				t.Fatalf("tool result = %s", encoded)
			}
		})
	}
}
