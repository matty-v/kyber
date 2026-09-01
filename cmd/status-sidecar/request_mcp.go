package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/requeststore"
	"github.com/matty-v/kyber/pkg/taskobject"
	"github.com/matty-v/kyber/pkg/taskstore"
)

const requestMCPProtocolVersion = "2025-06-18"

var requestReplyIDPattern = regexp.MustCompile(`^req_[a-f0-9]{32}$`)
var taskReplyIDPattern = regexp.MustCompile(`^task_[a-f0-9]{32}$`)
var taskAttemptIDPattern = regexp.MustCompile(`^attempt_[a-f0-9]{32}$`)
var taskUpdateIDPattern = regexp.MustCompile(`^update_[a-f0-9]{32}$`)
var taskResultIDPattern = regexp.MustCompile(`^result_[a-f0-9]{32}$`)

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

func taskResultToolSchema(extra map[string]any, requiredExtra string) map[string]any {
	properties := map[string]any{
		"task_id":     map[string]any{"type": "string", "pattern": `^task_[a-f0-9]{32}$`},
		"attempt_id":  map[string]any{"type": "string", "pattern": `^attempt_[a-f0-9]{32}$`},
		"result_id":   map[string]any{"type": "string", "pattern": `^result_[a-f0-9]{32}$`},
		"name":        map[string]any{"type": "string", "maxLength": taskstore.HardMaxResultNameBytes},
		"description": map[string]any{"type": "string", "maxLength": taskstore.HardMaxDescriptionBytes},
	}
	for name, schema := range extra {
		properties[name] = schema
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
		"required":             []string{"task_id", "attempt_id", "result_id", "name", requiredExtra},
	}
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
			"name":        "report_progress",
			"description": "Report bounded cooperative progress for the current durable task attempt.",
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"task_id":    map[string]any{"type": "string", "pattern": `^task_[a-f0-9]{32}$`},
					"attempt_id": map[string]any{"type": "string", "pattern": `^attempt_[a-f0-9]{32}$`},
					"update_id":  map[string]any{"type": "string", "pattern": `^update_[a-f0-9]{32}$`},
					"message":    map[string]any{"type": "string", "maxLength": taskstore.HardMaxProgressBytes},
					"percent":    map[string]any{"type": "integer", "minimum": 0, "maximum": 100},
				}, "required": []string{"task_id", "attempt_id", "update_id", "message"},
			},
		}, {
			"name":        "publish_text",
			"description": "Publish one immutable named UTF-8 text result for the current durable task attempt.",
			"inputSchema": taskResultToolSchema(map[string]any{"text": map[string]any{"type": "string", "maxLength": taskstore.HardMaxTextPartBytes}}, "text"),
		}, {
			"name":        "publish_json",
			"description": "Publish one immutable named JSON result for the current durable task attempt.",
			"inputSchema": taskResultToolSchema(map[string]any{"value": map[string]any{}}, "value"),
		}, {
			"name":        "publish_file",
			"description": "Publish one immutable named file result from the allowlisted task-results directory.",
			"inputSchema": taskResultToolSchema(map[string]any{
				"path":       map[string]any{"type": "string"},
				"filename":   map[string]any{"type": "string", "maxLength": taskstore.HardMaxFilenameBytes},
				"media_type": map[string]any{"type": "string", "maxLength": 255},
			}, "path"),
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
			RequestID   string          `json:"request_id"`
			TaskID      string          `json:"task_id"`
			AttemptID   string          `json:"attempt_id"`
			UpdateID    string          `json:"update_id"`
			ResultID    string          `json:"result_id"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Message     string          `json:"message"`
			Percent     *int            `json:"percent"`
			Text        string          `json:"text"`
			Value       json.RawMessage `json:"value"`
			Path        string          `json:"path"`
			Filename    string          `json:"filename"`
			MediaType   string          `json:"media_type"`
			Response    string          `json:"response"`
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
	if params.Name == "report_progress" {
		return s.reportTaskProgress(ctx, params.Arguments.TaskID, params.Arguments.AttemptID, params.Arguments.UpdateID, params.Arguments.Message, params.Arguments.Percent)
	}
	if params.Name == "publish_text" {
		return s.publishTaskResult(ctx, "text", params.Arguments.TaskID, params.Arguments.AttemptID, params.Arguments.ResultID, params.Arguments.Name, params.Arguments.Description, params.Arguments.Text, nil)
	}
	if params.Name == "publish_json" {
		return s.publishTaskResult(ctx, "json", params.Arguments.TaskID, params.Arguments.AttemptID, params.Arguments.ResultID, params.Arguments.Name, params.Arguments.Description, "", params.Arguments.Value)
	}
	if params.Name == "publish_file" {
		return s.publishTaskFile(ctx, params.Arguments.TaskID, params.Arguments.AttemptID, params.Arguments.ResultID, params.Arguments.Name, params.Arguments.Description, params.Arguments.Path, params.Arguments.Filename, params.Arguments.MediaType)
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

func (s *requestMCPServer) reportTaskProgress(ctx context.Context, taskID, attemptID, updateID, message string, percent *int) requestToolResult {
	taskID = strings.TrimSpace(taskID)
	attemptID = strings.TrimSpace(attemptID)
	updateID = strings.TrimSpace(updateID)
	if !taskReplyIDPattern.MatchString(taskID) || !taskAttemptIDPattern.MatchString(attemptID) || !taskUpdateIDPattern.MatchString(updateID) {
		return requestToolError("task, attempt, or update id is invalid")
	}
	if message == "" || len([]byte(message)) > taskstore.HardMaxProgressBytes || (percent != nil && (*percent < 0 || *percent > 100)) {
		return requestToolError("progress is invalid or exceeds the platform hard limit")
	}
	body, err := json.Marshal(map[string]any{"task_id": taskID, "attempt_id": attemptID, "update_id": updateID, "message": message, "percent": percent})
	if err != nil {
		return requestToolError("could not encode progress")
	}
	status, err := postToCP(ctx, s.client, s.cfg, "task-progress", body)
	if err != nil {
		return taskMutationError(status, "progress")
	}
	result := requestToolText("progress accepted")
	result.StructuredContent = map[string]any{"accepted": true, "task_id": taskID, "update_id": updateID}
	return result
}

func (s *requestMCPServer) publishTaskResult(ctx context.Context, kind, taskID, attemptID, resultID, name, description, text string, value json.RawMessage) requestToolResult {
	taskID = strings.TrimSpace(taskID)
	attemptID = strings.TrimSpace(attemptID)
	resultID = strings.TrimSpace(resultID)
	name = strings.TrimSpace(name)
	if !taskReplyIDPattern.MatchString(taskID) || !taskAttemptIDPattern.MatchString(attemptID) || !taskResultIDPattern.MatchString(resultID) {
		return requestToolError("task, attempt, or result id is invalid")
	}
	if name == "" || len([]byte(name)) > taskstore.HardMaxResultNameBytes || len([]byte(description)) > taskstore.HardMaxDescriptionBytes {
		return requestToolError("result metadata is invalid or exceeds the platform hard limit")
	}
	payload := map[string]any{"task_id": taskID, "attempt_id": attemptID, "result_id": resultID, "name": name, "description": description, "kind": kind}
	switch kind {
	case "text":
		if len([]byte(text)) > taskstore.HardMaxTextPartBytes {
			return requestToolError("text result exceeds the platform hard limit")
		}
		payload["text"] = text
	case "json":
		if len(value) == 0 || len(value) > taskstore.HardMaxJSONPartBytes || !json.Valid(value) {
			return requestToolError("JSON result is invalid or exceeds the platform hard limit")
		}
		payload["value"] = value
	default:
		return requestToolError("unknown result kind")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return requestToolError("could not encode result")
	}
	status, err := postToCP(ctx, s.client, s.cfg, "task-results", body)
	if err != nil {
		return taskMutationError(status, "result")
	}
	result := requestToolText("result accepted")
	result.StructuredContent = map[string]any{"accepted": true, "task_id": taskID, "result_id": resultID}
	return result
}

func (s *requestMCPServer) publishTaskFile(ctx context.Context, taskID, attemptID, resultID, name, description, path, filename, declaredType string) requestToolResult {
	taskID, attemptID, resultID = strings.TrimSpace(taskID), strings.TrimSpace(attemptID), strings.TrimSpace(resultID)
	name, path = strings.TrimSpace(name), strings.TrimSpace(path)
	if !taskReplyIDPattern.MatchString(taskID) || !taskAttemptIDPattern.MatchString(attemptID) || !taskResultIDPattern.MatchString(resultID) || name == "" {
		return requestToolError("task, attempt, result id, or name is invalid")
	}
	root := s.cfg.TaskResultsRoot
	if root == "" {
		root = taskobject.TaskResultsRoot
	}
	rel, err := taskResultRelativePath(root, path)
	if err != nil {
		return requestToolError("file path is outside the task-results directory")
	}
	file, err := taskobject.OpenTaskResult(root, rel, taskstore.HardMaxFileBytes)
	if err != nil {
		return requestToolError("file is unavailable or unsafe")
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	sniffedType, err := taskobject.SniffMediaType(reader)
	if err != nil {
		return requestToolError("file media type could not be inspected")
	}
	if filename == "" {
		filename = filepath.Base(rel)
	}
	metadata, err := json.Marshal(map[string]string{
		"task_id": taskID, "attempt_id": attemptID, "result_id": resultID,
		"name": name, "description": description, "filename": taskobject.SanitizeFilename(filename),
		"declared_media_type": strings.TrimSpace(declaredType), "sniffed_media_type": sniffedType,
	})
	if err != nil {
		return requestToolError("could not encode file metadata")
	}
	hash := sha256.New()
	url := fmt.Sprintf("%s/internal/agents/%s/task-files", s.cfg.ControlPlaneURL, s.cfg.AgentName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, io.TeeReader(reader, hash))
	if err != nil {
		return requestToolError("could not build file upload")
	}
	req.ContentLength = file.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Kyber-Task-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	if token, tokenErr := readPodToken(podTokenPath); tokenErr == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	uploadClient := *s.client
	uploadClient.Timeout = 2 * time.Minute
	resp, err := uploadClient.Do(req)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		defer resp.Body.Close()
	}
	if err != nil || status/100 != 2 {
		return taskMutationError(status, "file result")
	}
	var accepted struct {
		Result struct {
			Parts []struct {
				File *struct {
					SHA256 string `json:"sha256"`
				} `json:"file"`
			} `json:"parts"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&accepted); err != nil || len(accepted.Result.Parts) != 1 || accepted.Result.Parts[0].File == nil || accepted.Result.Parts[0].File.SHA256 != fmt.Sprintf("%x", hash.Sum(nil)) {
		return requestToolError("task file service returned mismatched content metadata")
	}
	result := requestToolText("file result accepted")
	result.StructuredContent = map[string]any{"accepted": true, "task_id": taskID, "result_id": resultID, "sha256": fmt.Sprintf("%x", hash.Sum(nil)), "size": file.Size(), "media_type": sniffedType}
	return result
}

func taskResultRelativePath(root, path string) (string, error) {
	if path == "" {
		return "", taskobject.ErrUnsafePath
	}
	if !filepath.IsAbs(path) {
		return path, nil
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", taskobject.ErrUnsafePath
	}
	return rel, nil
}

func taskMutationError(status int, noun string) requestToolResult {
	switch status {
	case http.StatusNotFound:
		return requestToolError("task or attempt was not found")
	case http.StatusConflict:
		return requestToolError("%s conflicts with current task state or prior idempotent content", noun)
	case http.StatusRequestEntityTooLarge:
		return requestToolError("%s exceeds the configured limit", noun)
	case http.StatusTooManyRequests:
		return requestToolError("task has reached its configured %s limit", noun)
	default:
		return requestToolError("task service is unavailable")
	}
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
