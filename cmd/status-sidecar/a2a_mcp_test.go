package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func callOutboundA2AMCP(t *testing.T, method string) requestRPCResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	(&outboundA2AMCPServer{}).handle(recorder, httptest.NewRequest(http.MethodPost, "/a2a/mcp", bytes.NewReader(body)))
	var response requestRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
}

func TestOutboundA2AMCPInitializesWithDistinctIdentity(t *testing.T) {
	response := callOutboundA2AMCP(t, "initialize")
	result, ok := response.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", response.Result)
	}
	server, ok := result["serverInfo"].(map[string]any)
	if !ok || server["name"] != "kyber-a2a" {
		t.Fatalf("serverInfo = %#v", result["serverInfo"])
	}
}

func TestOutboundA2AMCPPublishesBoundedToolSurface(t *testing.T) {
	tools := outboundA2ATools()
	want := []string{"discover_peer", "delegate_task", "get_task", "list_tasks", "await_task", "continue_task", "cancel_task", "download_artifact"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	for i, name := range want {
		if tools[i]["name"] != name {
			t.Errorf("tool[%d] = %q, want %q", i, tools[i]["name"], name)
		}
		schema := tools[i]["inputSchema"].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Errorf("tool %q schema is not closed", name)
		}
	}
}

func TestOutboundA2AMCPFailsClosedWithoutPeers(t *testing.T) {
	response := callOutboundA2AMCP(t, "tools/call")
	result, ok := response.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", response.Result)
	}
	if result["isError"] != true {
		t.Fatalf("result = %#v", result)
	}
}
