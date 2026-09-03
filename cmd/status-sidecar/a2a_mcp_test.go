package main

import (
	"bytes"
	"context"
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

func TestLoadOutboundA2APeers(t *testing.T) {
	t.Setenv("PEER_TOKEN", "secret-value")
	peers, err := loadOutboundA2APeers(`[{"name":"auditor","url":"https://agents.example/a2a/v1/agents/auditor","credentialEnv":"PEER_TOKEN"}]`)
	if err != nil {
		t.Fatalf("loadOutboundA2APeers: %v", err)
	}
	peer, ok := peers["auditor"]
	if !ok || peer.BaseURL.String() != "https://agents.example/a2a/v1/agents/auditor" || peer.Credential != "secret-value" {
		t.Fatalf("peer = %#v", peer)
	}
}

func TestLoadOutboundA2APeersRejectsUnsafeConfiguration(t *testing.T) {
	t.Setenv("PEER_TOKEN", "secret-value")
	tests := []struct {
		name string
		raw  string
	}{
		{"non-https URL", `[{"name":"auditor","url":"http://agents.example","credentialEnv":"PEER_TOKEN"}]`},
		{"URL userinfo", `[{"name":"auditor","url":"https://user@agents.example","credentialEnv":"PEER_TOKEN"}]`},
		{"invalid name", `[{"name":"Auditor","url":"https://agents.example","credentialEnv":"PEER_TOKEN"}]`},
		{"missing credential", `[{"name":"auditor","url":"https://agents.example","credentialEnv":"MISSING_TOKEN"}]`},
		{"duplicate name", `[{"name":"auditor","url":"https://one.example","credentialEnv":"PEER_TOKEN"},{"name":"auditor","url":"https://two.example","credentialEnv":"PEER_TOKEN"}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadOutboundA2APeers(tc.raw); err == nil {
				t.Fatal("loadOutboundA2APeers succeeded, want error")
			}
		})
	}
}

func TestOutboundA2AMCPDiscoversOnlyConfiguredPeer(t *testing.T) {
	server := &outboundA2AMCPServer{
		peers: map[string]outboundA2APeer{"auditor": {Name: "auditor"}},
		discover: func(_ context.Context, peer outboundA2APeer) (any, error) {
			return map[string]any{"name": peer.Name}, nil
		},
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"discover_peer","arguments":{"peer":"auditor"}}}`)
	recorder := httptest.NewRecorder()
	server.handle(recorder, httptest.NewRequest(http.MethodPost, "/a2a/mcp", bytes.NewReader(body)))
	var response requestRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response.Result.(map[string]any)
	if result["isError"] == true {
		t.Fatalf("result = %#v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	if structured["name"] != "auditor" {
		t.Fatalf("structuredContent = %#v", structured)
	}
}

func TestOutboundA2ADialRejectsProhibitedAddresses(t *testing.T) {
	for _, address := range []string{"127.0.0.1:443", "169.254.169.254:443", "10.0.0.1:443"} {
		t.Run(address, func(t *testing.T) {
			if _, err := outboundA2ADialContext(false)(context.Background(), "tcp", address); err == nil {
				t.Fatal("dial succeeded, want destination policy error")
			}
		})
	}
}
