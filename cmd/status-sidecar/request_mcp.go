package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/matty-v/kyber/pkg/requeststore"
	"github.com/matty-v/kyber/pkg/taskstore"
)

const requestMCPProtocolVersion = "2025-06-18"

var requestReplyIDPattern = regexp.MustCompile(`^req_[a-f0-9]{32}$`)
var taskReplyIDPattern = regexp.MustCompile(`^task_[a-f0-9]{32}$`)
var taskAttemptIDPattern = regexp.MustCompile(`^attempt_[a-f0-9]{32}$`)

type requestRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type requestRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type requestRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *requestRPCError `json:"error,omitempty"`
}

type requestToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type requestToolResult struct {
	Content           []requestToolContent `json:"content"`
	StructuredContent any                  `json:"structuredContent,omitempty"`
	IsError           bool                 `json:"isError,omitempty"`
}

type requestMCPServer struct {
	client *http.Client
	cfg    config
}

func (s *requestMCPServer) handle(w http.ResponseWriter, r *http.Request) {
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
			"serverInfo":      map[string]any{"name": "kyber-request-reply", "version": "1"},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": []map[string]any{{
			"name":        "get_self_profile",
			"description": "Read the current agent's safe self-profile: runtime, model, phase, requested resources, and installed skills.",
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{},
			},
		}, {
			"name":        "respond",
			"description": "Complete one dispatched Kyber agent request. Use only the request_id from the current kyber-request envelope.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string", "pattern": `^req_[a-f0-9]{32}$`},
					"response":   map[string]any{"type": "string"},
				},
				"required": []string{"request_id", "response"},
			},
		}, {
			"name":        "complete",
			"description": "Complete one dispatched durable Kyber task. Use only IDs from the current kyber-task envelope.",
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"task_id":    map[string]any{"type": "string", "pattern": `^task_[a-f0-9]{32}$`},
					"attempt_id": map[string]any{"type": "string", "pattern": `^attempt_[a-f0-9]{32}$`},
					"response":   map[string]any{"type": "string"},
				}, "required": []string{"task_id", "attempt_id", "response"},
			},
		}}}
	case "tools/call":
		response.Result = s.callTool(r.Context(), req.Params)
	default:
		response.Error = &requestRPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
	s.write(w, response)
}

func (s *requestMCPServer) callTool(ctx context.Context, raw json.RawMessage) requestToolResult {
	var params struct {
		Name      string `json:"name"`
		Arguments struct {
			RequestID string `json:"request_id"`
			TaskID    string `json:"task_id"`
			AttemptID string `json:"attempt_id"`
			Response  string `json:"response"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return requestToolError("invalid tool arguments")
	}
	if params.Name == "get_self_profile" {
		return s.getSelfProfile(ctx)
	}
	if params.Name == "complete" {
		return s.completeTask(ctx, params.Arguments.TaskID, params.Arguments.AttemptID, params.Arguments.Response)
	}
	if params.Name != "respond" {
		return requestToolError("unknown tool %q", params.Name)
	}
	params.Arguments.RequestID = strings.TrimSpace(params.Arguments.RequestID)
	if !requestReplyIDPattern.MatchString(params.Arguments.RequestID) {
		return requestToolError("request_id is invalid")
	}
	if len([]byte(params.Arguments.Response)) > requeststore.HardMaxResponseBytes {
		return requestToolError("response exceeds the platform hard limit")
	}
	body, err := json.Marshal(map[string]string{
		"request_id": params.Arguments.RequestID,
		"response":   params.Arguments.Response,
	})
	if err != nil {
		return requestToolError("could not encode response")
	}
	status, err := postToCP(ctx, s.client, s.cfg, "request-reply", body)
	if err != nil {
		switch status {
		case http.StatusNotFound:
			return requestToolError("request was not found or has expired")
		case http.StatusConflict:
			return requestToolError("request is not dispatched or already has a different response")
		case http.StatusRequestEntityTooLarge:
			return requestToolError("response exceeds the configured limit")
		default:
			return requestToolError("request reply service is unavailable")
		}
	}
	result := requestToolText("response accepted")
	result.StructuredContent = map[string]any{"accepted": true, "request_id": params.Arguments.RequestID}
	return result
}

func (s *requestMCPServer) getSelfProfile(ctx context.Context) requestToolResult {
	var profile map[string]any
	status, err := getFromCP(ctx, s.client, s.cfg, "self-profile", &profile)
	if err != nil {
		if status == http.StatusNotFound {
			return requestToolError("self profile was not found")
		}
		return requestToolError("self profile service is unavailable")
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return requestToolError("could not encode self profile")
	}
	result := requestToolText("%s", encoded)
	result.StructuredContent = profile
	return result
}

func (s *requestMCPServer) completeTask(ctx context.Context, taskID, attemptID, response string) requestToolResult {
	taskID = strings.TrimSpace(taskID)
	attemptID = strings.TrimSpace(attemptID)
	if !taskReplyIDPattern.MatchString(taskID) || !taskAttemptIDPattern.MatchString(attemptID) {
		return requestToolError("task or attempt id is invalid")
	}
	if response == "" {
		return requestToolError("response is required")
	}
	if len([]byte(response)) > taskstore.HardMaxResponseBytes {
		return requestToolError("response exceeds the platform hard limit")
	}
	body, err := json.Marshal(map[string]string{"task_id": taskID, "attempt_id": attemptID, "response": response})
	if err != nil {
		return requestToolError("could not encode response")
	}
	status, err := postToCP(ctx, s.client, s.cfg, "task-complete", body)
	if err != nil {
		switch status {
		case http.StatusNotFound:
			return requestToolError("task or attempt was not found")
		case http.StatusConflict:
			return requestToolError("task completion conflicts with current state")
		case http.StatusRequestEntityTooLarge:
			return requestToolError("response exceeds the configured limit")
		default:
			return requestToolError("task service is unavailable")
		}
	}
	result := requestToolText("task completion accepted")
	result.StructuredContent = map[string]any{"accepted": true, "task_id": taskID}
	return result
}

func requestToolText(format string, args ...any) requestToolResult {
	return requestToolResult{Content: []requestToolContent{{Type: "text", Text: fmt.Sprintf(format, args...)}}}
}

func requestToolError(format string, args ...any) requestToolResult {
	result := requestToolText(format, args...)
	result.IsError = true
	return result
}

func (s *requestMCPServer) write(w http.ResponseWriter, response requestRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
