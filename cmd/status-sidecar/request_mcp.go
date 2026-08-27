package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/matty-v/kyber/pkg/requeststore"
)

const requestMCPProtocolVersion = "2025-06-18"

var requestReplyIDPattern = regexp.MustCompile(`^req_[a-f0-9]{32}$`)

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
			Response  string `json:"response"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return requestToolError("invalid tool arguments")
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
