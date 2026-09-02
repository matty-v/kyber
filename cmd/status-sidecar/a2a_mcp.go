package main

import (
	"encoding/json"
	"net/http"
)

// outboundA2AMCPServer owns the runtime-facing A2A client boundary. Peer
// resolution and credentials deliberately stay behind this loopback service;
// the harness receives bounded peer names and task handles, never URLs or
// bearer values.
type outboundA2AMCPServer struct{}

func (s *outboundA2AMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req requestRPCRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		s.write(w, requestRPCResponse{JSONRPC: "2.0", Error: &requestRPCError{Code: -32700, Message: "parse error"}})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	response := requestRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": requestMCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kyber-a2a", "version": "1"},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": outboundA2ATools()}
	case "tools/call":
		response.Result = requestToolResult{
			Content: []requestToolContent{{Type: "text", Text: "outbound A2A peers are not configured for this agent"}},
			IsError: true,
		}
	default:
		response.Error = &requestRPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
	s.write(w, response)
}

func outboundA2ATools() []map[string]any {
	peer := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	taskID := map[string]any{"type": "string", "minLength": 1, "maxLength": 256}
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
	}
	tool := func(name, description string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "description": description, "inputSchema": schema}
	}
	return []map[string]any{
		tool("discover_peer", "Read the bounded Agent Card for an operator-configured peer.", object(map[string]any{"peer": peer}, "peer")),
		tool("delegate_task", "Delegate a new durable task to an operator-configured peer.", object(map[string]any{"peer": peer, "skill_id": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "context_id": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}}, "peer", "skill_id", "message")),
		tool("get_task", "Get a delegated task by durable handle.", object(map[string]any{"peer": peer, "task_id": taskID, "history_length": map[string]any{"type": "integer", "minimum": 0}, "include_artifacts": map[string]any{"type": "boolean"}}, "peer", "task_id")),
		tool("list_tasks", "List this source agent's tasks for one configured peer.", object(map[string]any{"peer": peer, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "peer")),
		tool("await_task", "Wait for bounded task progress using resumable A2A events.", object(map[string]any{"peer": peer, "task_id": taskID, "cursor": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60}}, "peer", "task_id")),
		tool("continue_task", "Continue an input-required delegated task.", object(map[string]any{"peer": peer, "task_id": taskID, "message": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}}, "peer", "task_id", "message")),
		tool("cancel_task", "Request cancellation of a delegated task.", object(map[string]any{"peer": peer, "task_id": taskID}, "peer", "task_id")),
		tool("download_artifact", "Download one authorized artifact part beneath the managed results directory.", object(map[string]any{"peer": peer, "task_id": taskID, "artifact_id": map[string]any{"type": "string"}, "part_index": map[string]any{"type": "integer", "minimum": 0}}, "peer", "task_id", "artifact_id", "part_index")),
	}
}

func (s *outboundA2AMCPServer) write(w http.ResponseWriter, response requestRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
