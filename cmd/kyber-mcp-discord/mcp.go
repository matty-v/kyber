package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const discordMCPProtocolVersion = "2025-06-18"

type discordRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type discordRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type discordRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *discordRPCError `json:"error,omitempty"`
}

type discordToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type discordToolResult struct {
	Content           []discordToolContent `json:"content"`
	StructuredContent any                  `json:"structuredContent,omitempty"`
	IsError           bool                 `json:"isError,omitempty"`
}

func discordTextResult(format string, args ...any) discordToolResult {
	return discordToolResult{Content: []discordToolContent{{Type: "text", Text: fmt.Sprintf(format, args...)}}}
}

func discordErrorResult(format string, args ...any) discordToolResult {
	return discordToolResult{Content: []discordToolContent{{Type: "text", Text: fmt.Sprintf(format, args...)}}, IsError: true}
}

type discordMCPServer struct {
	sender          discordSender
	allowedChannels map[string]bool
	threadParents   *sync.Map
	lifecycle       *discordLifecycle
	attachments     *attachmentStore
	client          *http.Client
	downloadDir     string
}

func (s *discordMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req discordRPCRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.write(w, discordRPCResponse{JSONRPC: "2.0", Error: &discordRPCError{Code: -32700, Message: "parse error"}})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := discordRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": discordMCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kyber-mcp-discord", "version": "1"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": []map[string]any{{
			"name":        "reply",
			"description": "Reply on Discord. Pass channel_id and message_id from the inbound message. message_id is optional and creates a Discord reply reference.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"channel_id": map[string]any{"type": "string", "description": "Target channel from the inbound Discord message."},
					"text":       map[string]any{"type": "string", "description": "Message body."},
					"message_id": map[string]any{"type": "string", "description": "Inbound message to reply to. Omit for a normal channel message."},
					"files":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Absolute file paths under /persist to attach."},
				},
				"required": []string{"channel_id", "text"},
			},
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message_id":  map[string]any{"type": "string", "description": "ID of the Discord message that was sent."},
					"message_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "All message IDs when a long reply is split into chunks."},
				},
				"required": []string{"message_id", "message_ids"},
			},
		}}}
		resp.Result = map[string]any{"tools": append(resp.Result.(map[string]any)["tools"].([]map[string]any), map[string]any{
			"name":        "download_attachment",
			"description": "Download an attachment from an accepted inbound Discord message into the shared /persist directory.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"attachment_id": map[string]any{"type": "string", "description": "Attachment ID from the inbound message."},
			}, "required": []string{"attachment_id"}},
		}, map[string]any{
			"name": "edit_message", "description": "Edit a Discord message this agent sent.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"channel_id": map[string]any{"type": "string"}, "message_id": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"},
			}, "required": []string{"channel_id", "message_id", "text"}},
		}, map[string]any{
			"name": "react", "description": "Add or remove the bot's emoji reaction on a Discord message.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"channel_id": map[string]any{"type": "string"}, "message_id": map[string]any{"type": "string"},
				"emoji":  map[string]any{"type": "string", "description": "Unicode emoji or custom emoji name:id."},
				"remove": map[string]any{"type": "boolean", "description": "Remove this bot's reaction instead of adding it."},
			}, "required": []string{"channel_id", "message_id", "emoji"}},
		})}
	case "tools/call":
		resp.Result = s.callTool(r.Context(), req.Params)
	default:
		resp.Error = &discordRPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
	s.write(w, resp)
}

func (s *discordMCPServer) write(w http.ResponseWriter, resp discordRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("discord-sidecar: writing MCP response", "error", err)
	}
}

func (s *discordMCPServer) callTool(ctx context.Context, raw json.RawMessage) discordToolResult {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return discordErrorResult("could not decode tool arguments: %v", err)
	}
	arg := func(key string) string {
		value, ok := params.Arguments[key]
		if !ok {
			return ""
		}
		v, ok := value.(string)
		if !ok {
			return ""
		}
		return strings.TrimSpace(v)
	}
	inScope := func(channelID string) bool {
		if len(s.allowedChannels) == 0 || s.allowedChannels[channelID] {
			return true
		}
		if s.threadParents != nil {
			if parent, ok := s.threadParents.Load(channelID); ok {
				return s.allowedChannels[parent.(string)]
			}
		}
		return false
	}
	if params.Name == "download_attachment" {
		attachmentID := arg("attachment_id")
		if attachmentID == "" {
			return discordErrorResult("attachment_id is required")
		}
		if s.attachments == nil {
			return discordErrorResult("attachment downloads are unavailable")
		}
		item, ok := s.attachments.get(attachmentID)
		if !ok {
			return discordErrorResult("attachment_id is not in scope for this agent — only attachments from accepted inbound messages may be downloaded")
		}
		path, err := downloadDiscordAttachment(ctx, s.client, item, s.downloadDir)
		if err != nil {
			return discordErrorResult("could not download attachment: %v", err)
		}
		return discordTextResult("downloaded to %s", path)
	}
	if params.Name == "edit_message" {
		channelID, messageID, text := arg("channel_id"), arg("message_id"), arg("text")
		if channelID == "" || messageID == "" || text == "" {
			return discordErrorResult("channel_id, message_id and text are required")
		}
		if !inScope(channelID) {
			return discordErrorResult("channel_id %s is not in scope for this agent", channelID)
		}
		if utf16Units(text) > discordMessageLimit {
			return discordErrorResult("edited text is %d UTF-16 units; Discord allows %d per message", utf16Units(text), discordMessageLimit)
		}
		message, err := s.sender.ChannelMessageEditComplex(discordgo.NewMessageEdit(channelID, messageID).SetContent(text))
		if err != nil {
			return discordErrorResult("Discord rejected the edit: %v", err)
		}
		id := messageID
		if message != nil && message.ID != "" {
			id = message.ID
		}
		result := discordTextResult("edited (id: %s)", id)
		result.StructuredContent = map[string]any{"message_id": id}
		return result
	}
	if params.Name == "react" {
		channelID, messageID, emoji := arg("channel_id"), arg("message_id"), arg("emoji")
		if channelID == "" || messageID == "" || emoji == "" {
			return discordErrorResult("channel_id, message_id and emoji are required")
		}
		if !inScope(channelID) {
			return discordErrorResult("channel_id %s is not in scope for this agent", channelID)
		}
		remove, _ := params.Arguments["remove"].(bool)
		var err error
		if remove {
			err = s.sender.MessageReactionRemove(channelID, messageID, emoji, "@me")
		} else {
			err = s.sender.MessageReactionAdd(channelID, messageID, emoji)
		}
		if err != nil {
			return discordErrorResult("Discord rejected the reaction: %v", err)
		}
		if remove {
			return discordTextResult("reaction removed")
		}
		return discordTextResult("reaction added")
	}
	if params.Name != "reply" {
		return discordErrorResult("unknown tool: %s", params.Name)
	}
	channelID, text, messageID := arg("channel_id"), arg("text"), arg("message_id")
	if channelID == "" || text == "" {
		return discordErrorResult("channel_id and text are required")
	}
	if !inScope(channelID) {
		return discordErrorResult("channel_id %s is not in scope for this agent", channelID)
	}
	var files []string
	if rawFiles, ok := params.Arguments["files"].([]any); ok {
		for _, rawFile := range rawFiles {
			path, ok := rawFile.(string)
			if !ok || strings.TrimSpace(path) == "" {
				continue
			}
			validated, err := validateDiscordOutboundFile(strings.TrimSpace(path), []string{"/persist"})
			if err != nil {
				return discordErrorResult("attachment %s is not allowed: %v", filepath.Base(path), err)
			}
			files = append(files, validated)
		}
	}
	outcome, err := sendDiscordMessage(s.sender, channelID, text, messageID, files)
	if err != nil {
		slog.Warn("discord-sidecar: MCP reply failed", "channel", channelID, "error", err)
		if len(outcome.MessageIDs) > 0 {
			return discordErrorResult("Discord sent %d chunk(s) (%s) before the reply failed: %v; do not resend those chunks",
				len(outcome.MessageIDs), strings.Join(outcome.MessageIDs, ", "), err)
		}
		return discordErrorResult("Discord rejected the message: %v", err)
	}
	firstID := outcome.MessageIDs[0]
	result := discordTextResult("sent (%d message(s); ids: %s)", len(outcome.MessageIDs), strings.Join(outcome.MessageIDs, ", "))
	result.StructuredContent = map[string]any{"message_id": firstID, "message_ids": outcome.MessageIDs}
	if s.lifecycle != nil {
		s.lifecycle.complete(channelID, messageID)
	}
	return result
}

func startDiscordMCPServer(addr string, sender discordSender, cfg *config) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", (&discordMCPServer{
		sender: sender, allowedChannels: cfg.allowedChannels, lifecycle: cfg.lifecycle,
		attachments: cfg.attachments, threadParents: cfg.threadParents, client: newDiscordAttachmentClient(60 * time.Second), downloadDir: cfg.downloadDir,
	}).handle)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("discord-sidecar: MCP server", "error", err)
		}
	}()
	return srv
}
