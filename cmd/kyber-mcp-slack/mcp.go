package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	button := map[string]any{"type": "object", "properties": map[string]any{"text": str, "value": str, "action_id": str}, "required": []string{"text", "value"}}
	return []map[string]any{{"name": "reply", "description": "Reply in an allowlisted Slack conversation.", "inputSchema": schema(map[string]any{"channel_id": str, "text": str, "thread_ts": str, "files": map[string]any{"type": "array", "items": str, "description": "Absolute file paths under /persist to attach."}, "buttons": map[string]any{"type": "array", "items": button, "description": "Interactive Block Kit buttons."}}, "channel_id", "text")}, {"name": "edit_message", "description": "Edit a Slack message sent by this bot; pass buttons to replace controls or [] to clear them.", "inputSchema": schema(map[string]any{"channel_id": str, "message_id": str, "text": str, "buttons": map[string]any{"type": "array", "items": button}}, "channel_id", "message_id", "text")}, {"name": "react", "description": "Add or remove this bot's emoji reaction.", "inputSchema": schema(map[string]any{"channel_id": str, "message_id": str, "emoji": str, "remove": map[string]any{"type": "boolean"}}, "channel_id", "message_id", "emoji")}, {"name": "download_attachment", "description": "Download a file from an accepted inbound Slack message into /persist.", "inputSchema": schema(map[string]any{"file_id": str}, "file_id")}}
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
	var callbackTokens []string
	callbackReplaceMessage := ""
	switch p.Name {
	case "reply":
		if arg("text") == "" {
			return toolError("channel_id and text are required")
		}
		files := stringArgs(p.Arguments["files"])
		if len(files) > 0 {
			if buttons, _ := p.Arguments["buttons"].([]any); len(buttons) > 0 {
				return toolError("files and buttons cannot be sent in the same Slack reply")
			}
			return s.uploadFiles(ctx, ch, arg("thread_ts"), arg("text"), files)
		}
		return s.sendReply(ctx, ch, arg("thread_ts"), arg("text"), p.Arguments["buttons"])
	case "edit_message":
		if arg("message_id") == "" || arg("text") == "" {
			return toolError("channel_id, message_id and text are required")
		}
		method = "chat.update"
		body["ts"] = arg("message_id")
		body["text"] = arg("text")
		if buttons, present := p.Arguments["buttons"]; present {
			if s.cfg.callbacks == nil {
				return toolError("interactive callbacks are unavailable")
			}
			blocks, tokens, err := s.cfg.callbacks.register(ch, buttons)
			if err != nil {
				return toolError("invalid buttons: " + err.Error())
			}
			callbackTokens = tokens
			callbackReplaceMessage = arg("message_id")
			if blocks == nil {
				body["blocks"] = []any{}
			} else {
				body["blocks"] = blocks
			}
		}
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
		if s.cfg.callbacks != nil {
			s.cfg.callbacks.remove(callbackTokens)
		}
		return toolError("Slack rejected the request: " + err.Error())
	}
	ts, _ := out["ts"].(string)
	r := result("request completed")
	if ts != "" {
		if callbackReplaceMessage != "" && s.cfg.callbacks != nil {
			s.cfg.callbacks.removeForMessage(ch, callbackReplaceMessage)
		}
		if s.cfg.callbacks != nil {
			s.cfg.callbacks.bind(callbackTokens, ts)
		}
		r.Content[0]["text"] = "sent (id: " + ts + ")"
		r.StructuredContent = map[string]any{"message_id": ts}
	}
	return r
}

const slackMessageLimit = 4000

func splitSlackText(text string) []string {
	runes := []rune(text)
	if len(runes) <= slackMessageLimit {
		return []string{text}
	}
	parts := make([]string, 0, len(runes)/slackMessageLimit+1)
	for len(runes) > 0 {
		end := min(slackMessageLimit, len(runes))
		if end < len(runes) {
			for i := end; i > end-400 && i > 0; i-- {
				if runes[i-1] == '\n' || runes[i-1] == ' ' {
					end = i
					break
				}
			}
		}
		parts = append(parts, strings.TrimSpace(string(runes[:end])))
		runes = runes[end:]
	}
	return parts
}

func (s *mcpServer) sendReply(ctx context.Context, channel, threadTS, text string, buttons any) slackToolResult {
	chunks := splitSlackText(text)
	ids := make([]string, 0, len(chunks))
	var tokens []string
	for i, chunk := range chunks {
		body := map[string]any{"channel": channel, "text": chunk}
		if threadTS != "" {
			body["thread_ts"] = threadTS
		}
		if i == len(chunks)-1 && buttons != nil {
			if s.cfg.callbacks == nil {
				return toolError("interactive callbacks are unavailable")
			}
			blocks, registered, err := s.cfg.callbacks.register(channel, buttons)
			if err != nil {
				return toolError("invalid buttons: " + err.Error())
			}
			tokens = registered
			if len(blocks) > 0 {
				section := map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": chunk}}
				body["blocks"] = append([]map[string]any{section}, blocks...)
			}
		}
		out, err := s.api(ctx, "chat.postMessage", body)
		if err != nil {
			if s.cfg.callbacks != nil {
				s.cfg.callbacks.remove(tokens)
			}
			r := toolError(fmt.Sprintf("Slack rejected chunk %d of %d after %d successful sends: %v; do not resend the completed chunks", i+1, len(chunks), len(ids), err))
			r.StructuredContent = map[string]any{"message_ids": ids, "partial": len(ids) > 0}
			return r
		}
		ts, _ := out["ts"].(string)
		if ts != "" {
			ids = append(ids, ts)
			if s.cfg.callbacks != nil {
				s.cfg.callbacks.bind(tokens, ts)
			}
		}
	}
	if len(ids) == 0 {
		return toolError("Slack accepted the reply but returned no message identifier")
	}
	r := result("sent (id: " + ids[len(ids)-1] + ")")
	r.StructuredContent = map[string]any{"message_id": ids[len(ids)-1], "message_ids": ids}
	return r
}

func stringArgs(value any) []string {
	raw, _ := value.([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if path, ok := item.(string); ok && strings.TrimSpace(path) != "" {
			out = append(out, strings.TrimSpace(path))
		}
	}
	return out
}

func (s *mcpServer) uploadFiles(ctx context.Context, channel, threadTS, text string, paths []string) slackToolResult {
	files := make([]map[string]string, 0, len(paths))
	for _, path := range paths {
		resolved, info, err := validateSlackOutboundFile(path)
		if err != nil {
			return toolError("could not upload file: " + err.Error())
		}
		start, err := s.api(ctx, "files.getUploadURLExternal", map[string]any{"filename": info.Name(), "length": info.Size()})
		if err != nil {
			return toolError("Slack rejected the upload: " + err.Error())
		}
		uploadURL, _ := start["upload_url"].(string)
		fileID, _ := start["file_id"].(string)
		if uploadURL == "" || fileID == "" {
			return toolError("Slack returned an incomplete upload reservation")
		}
		if err := uploadSlackFile(ctx, s.client, uploadURL, resolved); err != nil {
			return toolError("could not upload file: " + err.Error())
		}
		files = append(files, map[string]string{"id": fileID, "title": info.Name()})
	}
	body := map[string]any{"files": files, "channel_id": channel, "initial_comment": text}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	if _, err := s.api(ctx, "files.completeUploadExternal", body); err != nil {
		return toolError("Slack rejected the upload: " + err.Error())
	}
	return result("uploaded " + fmt.Sprint(len(files)) + " file(s)")
}
func (s *mcpServer) api(ctx context.Context, method string, body map[string]any) (map[string]any, error) {
	return s.apiAttempt(ctx, method, body, true)
}

func (s *mcpServer) apiAttempt(ctx context.Context, method string, body map[string]any, retry bool) (map[string]any, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", slackAPIBaseURL+method, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.cfg.botToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", method, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		if !retry {
			return nil, fmt.Errorf("rate_limited")
		}
		seconds, _ := strconv.Atoi(res.Header.Get("Retry-After"))
		if seconds < 1 {
			seconds = 1
		}
		if seconds > 5 {
			seconds = 5
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(seconds) * time.Second):
		}
		return s.apiAttempt(ctx, method, body, false)
	}
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
