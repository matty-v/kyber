package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	for _, want := range []string{`"name":"respond"`, `"name":"complete"`, `"request_id"`, `"task_id"`, `"attempt_id"`, `"response"`} {
		if !bytes.Contains(encoded, []byte(want)) {
			t.Fatalf("tools/list missing %s: %s", want, encoded)
		}
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
