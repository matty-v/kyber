package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const slackMCPProtocolVersion = "2025-06-18"

var slackAPIBaseURL = "https://slack.com/api/"

type mcpServer struct {
	cfg    config
	client *http.Client
}

func newMCPServer(c config, h *http.Client) *mcpServer { return &mcpServer{c, h} }
func (s *mcpServer) channelAllowed(ch string) bool {
	return ch != "" && len(s.cfg.channels) > 0 && s.cfg.channels[ch]
}

type slackToolResult struct {
	Content           []map[string]string `json:"content"`
	StructuredContent any                 `json:"structuredContent,omitempty"`
	IsError           bool                `json:"isError,omitempty"`
}

func result(t string) slackToolResult {
	return slackToolResult{Content: []map[string]string{{"type": "text", "text": t}}}
}
func toolError(t string) slackToolResult { r := result(t); r.IsError = true; return r }
func (s *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var q struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q) != nil {
		return
	}
	if len(q.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	out := map[string]any{"jsonrpc": "2.0", "id": q.ID}
	switch q.Method {
	case "initialize":
		out["result"] = map[string]any{"protocolVersion": slackMCPProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "kyber-mcp-slack", "version": "2"}}
	case "ping":
		out["result"] = map[string]any{}
	case "tools/list":
		out["result"] = map[string]any{"tools": toolDefs()}
	case "tools/call":
		out["result"] = s.call(r.Context(), q.Params)
	default:
		out["error"] = map[string]any{"code": -32601, "message": "method not found: " + q.Method}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
func toolDefs() []map[string]any {
	str := map[string]any{"type": "string"}
	schema := func(p map[string]any, req ...string) map[string]any {
		return map[string]any{"type": "object", "properties": p, "required": req}
	}
	return []map[string]any{{"name": "reply", "description": "Reply in an allowlisted Slack conversation.", "inputSchema": schema(map[string]any{"channel_id": str, "text": str, "thread_ts": str}, "channel_id", "text")}, {"name": "edit_message", "description": "Edit a Slack message sent by this bot.", "inputSchema": schema(map[string]any{"channel_id": str, "message_id": str, "text": str}, "channel_id", "message_id", "text")}, {"name": "react", "description": "Add or remove this bot's emoji reaction.", "inputSchema": schema(map[string]any{"channel_id": str, "message_id": str, "emoji": str, "remove": map[string]any{"type": "boolean"}}, "channel_id", "message_id", "emoji")}, {"name": "download_attachment", "description": "Download a file from an accepted inbound Slack message into /persist.", "inputSchema": schema(map[string]any{"file_id": str}, "file_id")}}
}
func (s *mcpServer) call(ctx context.Context, raw json.RawMessage) slackToolResult {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return toolError("could not decode tool arguments")
	}
	arg := func(k string) string { v, _ := p.Arguments[k].(string); return strings.TrimSpace(v) }
	if p.Name == "download_attachment" {
		fileID := arg("file_id")
		if fileID == "" {
			return toolError("file_id is required")
		}
		if s.cfg.attachments == nil {
			return toolError("attachment downloads are unavailable")
		}
		item, ok := s.cfg.attachments.get(fileID)
		if !ok {
			return toolError("file_id is not in scope for this agent — only files from accepted inbound messages may be downloaded")
		}
		path, err := downloadSlackFile(ctx, newSlackFileClient(s.cfg.botToken), s.cfg.botToken, item, s.cfg.downloadDir)
		if err != nil {
			return toolError("could not download attachment: " + err.Error())
		}
		return result("downloaded to " + path)
	}
	ch := arg("channel_id")
	if !s.channelAllowed(ch) {
		return toolError("channel_id is not in scope for this agent")
	}
	body := map[string]any{"channel": ch}
	method := ""
	switch p.Name {
	case "reply":
		if arg("text") == "" {
			return toolError("channel_id and text are required")
		}
		method = "chat.postMessage"
		body["text"] = arg("text")
		if arg("thread_ts") != "" {
			body["thread_ts"] = arg("thread_ts")
		}
	case "edit_message":
		if arg("message_id") == "" || arg("text") == "" {
			return toolError("channel_id, message_id and text are required")
		}
		method = "chat.update"
		body["ts"] = arg("message_id")
		body["text"] = arg("text")
	case "react":
		if arg("message_id") == "" || arg("emoji") == "" {
			return toolError("channel_id, message_id and emoji are required")
		}
		method = "reactions.add"
		if remove, _ := p.Arguments["remove"].(bool); remove {
			method = "reactions.remove"
		}
		body["timestamp"] = arg("message_id")
		body["name"] = strings.Trim(arg("emoji"), ":")
	default:
		return toolError("unknown tool: " + p.Name)
	}
	out, err := s.api(ctx, method, body)
	if err != nil {
		return toolError("Slack rejected the request: " + err.Error())
	}
	ts, _ := out["ts"].(string)
	r := result("request completed")
	if ts != "" {
		r.Content[0]["text"] = "sent (id: " + ts + ")"
		r.StructuredContent = map[string]any{"message_id": ts}
	}
	return r
}
func (s *mcpServer) api(ctx context.Context, method string, body map[string]any) (map[string]any, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", slackAPIBaseURL+method, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.cfg.botToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", method, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", method, err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		reason, _ := out["error"].(string)
		return nil, fmt.Errorf("%s", reason)
	}
	return out, nil
}
