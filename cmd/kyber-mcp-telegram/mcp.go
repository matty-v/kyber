package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MCP server (kyber#684). Until now this binary was "mcp" in name only: it
// exposed /healthz and a bare /send, so a Claude Code agent moved onto it would
// have lost the tool interface its plugin gives it (reply, react, edit_message,
// download_attachment) and had to curl instead. Matt chose Option A — keep the
// tool surface — so the sidecar speaks real MCP.
//
// Transport is Streamable HTTP on loopback, NOT stdio: the agent runs in a
// different container from this sidecar, so there is no process to spawn. The
// Claude Code actually deployed on the fleet supports it
// (`claude mcp add --transport http`), verified in-pod before this was built.
//
// Auth is the pod boundary. The listener binds 127.0.0.1 and both containers
// share a network namespace, so only this pod can reach it — the same trust
// boundary /send already relied on.

// protocolVersion is the MCP revision this server implements. Clients send
// their own in `initialize`; we echo ours and let the client decide.
const protocolVersion = "2025-06-18"

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// toolContent is one item of an MCP tool result.
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

func textResult(format string, args ...any) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: fmt.Sprintf(format, args...)}}}
}

// errorResult reports a tool failure to the MODEL rather than as a transport
// error, which is what lets the agent read the reason and adapt (bad
// parse_mode, chat out of scope) instead of seeing an opaque RPC failure.
func errorResult(format string, args ...any) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: fmt.Sprintf(format, args...)}}, IsError: true}
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// toolDefs is the advertised tool surface. Deliberately mirrors the Claude Code
// Telegram plugin's names and shapes so migrated agents' identity instructions
// ("reply with the reply tool", "use react") keep meaning the same thing.
func toolDefs(downloadDir string) []toolDef {
	return []toolDef{
		{
			Name: "reply",
			Description: "Reply on Telegram. Pass chat_id from the inbound message. Optionally pass reply_to " +
				"(message_id) to thread under an earlier message, and files (absolute paths) to attach.",
			InputSchema: obj(map[string]any{
				"chat_id": str("Target chat. Must be a chat this agent has received a message from."),
				"text":    str("Message body."),
				"format": map[string]any{
					"type": "string", "enum": []string{"text", "markdownv2"},
					"description": "Rendering mode. 'markdownv2' enables Telegram formatting; the caller must escape " +
						"special characters per MarkdownV2 rules. Default: 'text'.",
				},
				"reply_to": str("message_id to thread under. Omit for a normal reply."),
				"buttons": map[string]any{
					"type": "array", "maxItems": maxTelegramButtons, "items": map[string]any{
						"type": "object", "properties": map[string]any{
							"text":  str("Button label shown to the user."),
							"value": str("Opaque value returned to the agent when selected."),
						}, "required": []string{"text", "value"},
					},
					"description": "Inline action buttons. Each button is rendered on its own row.",
				},
				"files": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"},
					"description": "Absolute paths under /persist to attach using Telegram-native media methods where supported.",
				},
			}, "chat_id", "text"),
		},
		{
			Name:        "react",
			Description: "Add an emoji reaction to a message. Pass an empty emoji to clear the reaction.",
			InputSchema: obj(map[string]any{
				"chat_id":    str("Chat containing the message."),
				"message_id": str("Message to react to."),
				"emoji":      str("A single emoji, e.g. 👍. Empty string clears."),
			}, "chat_id", "message_id"),
		},
		{
			Name: "edit_message",
			Description: "Edit a message this agent already sent. Omit buttons to preserve its keyboard, " +
				"pass buttons: [] to remove it, or pass new buttons to replace it. Edits do NOT trigger push notifications — " +
				"when a long task completes, send a new reply so the user's device pings.",
			InputSchema: obj(map[string]any{
				"chat_id":    str("Chat containing the message."),
				"message_id": str("Message to edit (from a previous reply's result)."),
				"text":       str("Replacement body."),
				"format": map[string]any{
					"type": "string", "enum": []string{"text", "markdownv2"},
					"description": "Rendering mode. Default: 'text'.",
				},
				"buttons": map[string]any{
					"type": "array", "maxItems": maxTelegramButtons, "items": map[string]any{
						"type": "object", "properties": map[string]any{
							"text": str("Button label shown to the user."), "value": str("Value returned when selected."),
						}, "required": []string{"text", "value"},
					},
					"description": "Replacement inline buttons. An empty array removes all buttons; omit to preserve them.",
				},
			}, "chat_id", "message_id", "text"),
		},
		{
			Name: "download_attachment",
			Description: "Download a file the user attached, using the file_id from the inbound message. " +
				"Returns a local path under " + downloadDir + " that you can then read.",
			InputSchema: obj(map[string]any{
				"file_id": str("file_id from the inbound message's attachment."),
			}, "file_id"),
		},
	}
}

// mcpServer handles JSON-RPC over HTTP for one agent.
type mcpServer struct {
	cfg         *config
	client      *http.Client
	downloadDir string
	uploadRoots []string
}

func (s *mcpServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// GET is the SSE half of Streamable HTTP. We never push server-initiated
		// messages, so decline it explicitly rather than leaving a client
		// waiting on a stream that will never carry anything.
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req jsonRPCRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeRPC(w, jsonRPCResponse{JSONRPC: "2.0", Error: &jsonRPCError{Code: -32700, Message: "parse error"}})
		return
	}

	// A request without an id is a notification: act on it, answer nothing.
	// Returning a body here makes strict clients treat the response as
	// unsolicited and drop the connection.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kyber-mcp-telegram", "version": "1"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolDefs(s.downloadDir)}
	case "tools/call":
		resp.Result = s.callTool(r.Context(), req.Params)
	default:
		resp.Error = &jsonRPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp jsonRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("telegram-sidecar: writing MCP response", "error", err)
	}
}

func (s *mcpServer) callTool(ctx context.Context, raw json.RawMessage) toolResult {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return errorResult("could not decode tool arguments: %v", err)
	}
	arg := func(key string) string {
		if v, ok := params.Arguments[key]; ok {
			if sv, ok := v.(string); ok {
				return strings.TrimSpace(sv)
			}
			// Telegram ids are numeric and a model may well send them as JSON
			// numbers; accept that rather than failing on a type technicality.
			if fv, ok := v.(float64); ok {
				return strconv.FormatInt(int64(fv), 10)
			}
		}
		return ""
	}
	// parse_mode: the tool takes a friendly enum, Telegram wants its own name.
	parseMode := func() string {
		if strings.EqualFold(arg("format"), "markdownv2") {
			return "MarkdownV2"
		}
		return ""
	}

	switch params.Name {
	case "reply":
		chatID, text := arg("chat_id"), arg("text")
		if chatID == "" || text == "" {
			return errorResult("chat_id and text are required")
		}
		if !s.inScope(chatID) {
			return errorResult("chat_id %s is not in scope for this agent — you can only reply to chats that have messaged you", chatID)
		}
		var files []string
		if raw, ok := params.Arguments["files"].([]any); ok {
			for _, f := range raw {
				if sf, ok := f.(string); ok && strings.TrimSpace(sf) != "" {
					files = append(files, strings.TrimSpace(sf))
				}
			}
		}
		validatedFiles := make([]string, 0, len(files))
		for _, path := range files {
			validated, err := validateOutboundFile(path, s.uploadRoots)
			if err != nil {
				return errorResult("attachment %s is not allowed: %v", filepath.Base(path), err)
			}
			validatedFiles = append(validatedFiles, validated)
		}
		var buttons []callbackButton
		if rawButtons, ok := params.Arguments["buttons"].([]any); ok {
			for _, rawButton := range rawButtons {
				button, ok := rawButton.(map[string]any)
				if !ok {
					return errorResult("each button must be an object with text and value")
				}
				label, _ := button["text"].(string)
				value, _ := button["value"].(string)
				buttons = append(buttons, callbackButton{Text: strings.TrimSpace(label), Value: strings.TrimSpace(value)})
			}
		}
		var replyMarkup string
		var callbackTokens []string
		if len(buttons) > 0 {
			if s.cfg.callbacks == nil {
				return errorResult("inline buttons are unavailable")
			}
			var err error
			replyMarkup, callbackTokens, err = s.cfg.callbacks.register(chatID, buttons)
			if err != nil {
				return errorResult("could not create inline buttons: %v", err)
			}
		}
		msg, err := sendMessage(ctx, s.cfg, s.client, chatID, text, parseMode(), arg("reply_to"), replyMarkup)
		if err != nil {
			s.cfg.callbacks.remove(callbackTokens)
			slog.Warn("telegram-sidecar: reply failed", "chat_id", chatID, "error", err)
			return errorResult("Telegram rejected the message: %v", err)
		}
		if len(callbackTokens) > 0 {
			s.cfg.callbacks.bindMessage(callbackTokens, strconv.FormatInt(msg.MessageID, 10))
		}
		if s.cfg.actions != nil {
			s.cfg.actions.stop(chatID)
		}
		// Attachments follow the text. A failed attachment must not read as a
		// failed reply — the text already landed and the model needs to know
		// that, or it will send the whole thing again.
		var failed []string
		albumPaths := validatedFiles
		if len(albumPaths) > 10 {
			albumPaths = nil
		}
		for _, path := range albumPaths {
			if nativeAlbumMediaType(path) == "" {
				albumPaths = nil
				break
			}
		}
		if len(albumPaths) >= 2 {
			if err := sendMediaGroup(ctx, s.cfg, s.client, chatID, albumPaths); err != nil {
				failed = append(failed, fmt.Sprintf("media group (%v)", err))
			}
			validatedFiles = nil
		}
		for _, path := range validatedFiles {
			if _, err := sendFile(ctx, s.cfg, s.client, chatID, path, "", ""); err != nil {
				slog.Warn("telegram-sidecar: attachment failed", "path", path, "error", err)
				failed = append(failed, fmt.Sprintf("%s (%v)", path, err))
			}
		}
		if len(failed) > 0 {
			return errorResult("message sent (id: %d) but %d attachment(s) failed: %s",
				msg.MessageID, len(failed), strings.Join(failed, "; "))
		}
		return textResult("sent (id: %d)", msg.MessageID)

	case "react":
		chatID, messageID := arg("chat_id"), arg("message_id")
		if chatID == "" || messageID == "" {
			return errorResult("chat_id and message_id are required")
		}
		if !s.inScope(chatID) {
			return errorResult("chat_id %s is not in scope for this agent", chatID)
		}
		if err := setMessageReaction(ctx, s.cfg, s.client, chatID, messageID, arg("emoji")); err != nil {
			return errorResult("Telegram rejected the reaction: %v", err)
		}
		return textResult("reaction set")

	case "edit_message":
		chatID, messageID, text := arg("chat_id"), arg("message_id"), arg("text")
		if chatID == "" || messageID == "" || text == "" {
			return errorResult("chat_id, message_id and text are required")
		}
		if !s.inScope(chatID) {
			return errorResult("chat_id %s is not in scope for this agent", chatID)
		}
		replyMarkup, callbackTokens, setReplyMarkup := "", []string(nil), false
		if rawButtons, present := params.Arguments["buttons"]; present {
			setReplyMarkup = true
			buttons, ok := rawButtons.([]any)
			if !ok {
				return errorResult("buttons must be an array")
			}
			if len(buttons) == 0 {
				replyMarkup = `{"inline_keyboard":[]}`
			} else {
				parsed := make([]callbackButton, 0, len(buttons))
				for _, rawButton := range buttons {
					button, ok := rawButton.(map[string]any)
					if !ok {
						return errorResult("each button must be an object with text and value")
					}
					label, _ := button["text"].(string)
					value, _ := button["value"].(string)
					parsed = append(parsed, callbackButton{Text: strings.TrimSpace(label), Value: strings.TrimSpace(value)})
				}
				var err error
				replyMarkup, callbackTokens, err = s.cfg.callbacks.register(chatID, parsed)
				if err != nil {
					return errorResult("could not create inline buttons: %v", err)
				}
			}
		}
		if err := editMessageText(ctx, s.cfg, s.client, chatID, messageID, text, parseMode(), replyMarkup, setReplyMarkup); err != nil {
			s.cfg.callbacks.remove(callbackTokens)
			return errorResult("Telegram rejected the edit: %v", err)
		}
		if setReplyMarkup {
			s.cfg.callbacks.removeForMessage(chatID, messageID)
			s.cfg.callbacks.bindMessage(callbackTokens, messageID)
		}
		if s.cfg.actions != nil {
			s.cfg.actions.stop(chatID)
		}
		return textResult("edited")

	case "download_attachment":
		fileID := arg("file_id")
		if fileID == "" {
			return errorResult("file_id is required")
		}
		if s.cfg.files == nil || !s.cfg.files.has(fileID) {
			return errorResult("file_id is not in scope for this agent — only files from allowlisted inbound messages may be downloaded")
		}
		path, err := downloadFile(ctx, s.cfg, s.client, fileID, s.downloadDir)
		if err != nil {
			return errorResult("could not download attachment: %v", err)
		}
		return textResult("downloaded to %s", path)
	}
	return errorResult("unknown tool: %s", params.Name)
}

// inScope mirrors the /send guard: outbound is bounded to chats that have
// demonstrably reached this agent, so a prompt-injected model cannot use the
// bot to message arbitrary chats.
func (s *mcpServer) inScope(chatID string) bool {
	return s.cfg.chats == nil || s.cfg.chats.has(chatID)
}

func startMCPServer(addr string, cfg *config, client *http.Client, downloadDir string) *http.Server {
	srv := &mcpServer{cfg: cfg, client: client, downloadDir: downloadDir, uploadRoots: []string{"/persist"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", srv.handle)
	return startHTTPServer("mcp", addr, mux)
}

// httpClientForTools is separate from the poll client: getUpdates deliberately
// long-polls for 30s, and a tool call must not inherit that patience.
func httpClientForTools() *http.Client { return &http.Client{Timeout: 60 * time.Second} }
