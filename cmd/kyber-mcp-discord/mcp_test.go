package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func discordRPC(t *testing.T, s *discordMCPServer, method string, params any) discordRPCResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	resp := httptest.NewRecorder()
	s.handle(resp, req)
	var decoded discordRPCResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, resp.Body.String())
	}
	return decoded
}

func TestDiscordMCPReplyAllowsObservedThread(t *testing.T) {
	parents := &sync.Map{}
	parents.Store("thread", "parent")
	fake := &fakeDiscordSender{}
	s := &discordMCPServer{sender: fake, allowedChannels: map[string]bool{"parent": true}, threadParents: parents}
	resp := discordRPC(t, s, "tools/call", map[string]any{
		"name": "reply", "arguments": map[string]any{"channel_id": "thread", "text": "in thread"},
	})
	raw, _ := json.Marshal(resp.Result)
	if strings.Contains(string(raw), "not in scope") || fake.channelID != "thread" {
		t.Fatalf("thread reply result=%s channel=%q", raw, fake.channelID)
	}
}

func TestDiscordMCPInitializeAndToolsList(t *testing.T) {
	s := &discordMCPServer{sender: &fakeDiscordSender{}}
	init := discordRPC(t, s, "initialize", map[string]any{})
	if init.Error != nil {
		t.Fatalf("initialize error = %+v", init.Error)
	}
	list := discordRPC(t, s, "tools/list", nil)
	raw, _ := json.Marshal(list.Result)
	if !strings.Contains(string(raw), `"name":"reply"`) || !strings.Contains(string(raw), `"channel_id"`) {
		t.Fatalf("tools/list = %s", raw)
	}
}

func TestDiscordMCPReply(t *testing.T) {
	sender := &fakeDiscordSender{}
	s := &discordMCPServer{sender: sender, allowedChannels: map[string]bool{"123": true}}
	result := discordRPC(t, s, "tools/call", map[string]any{
		"name": "reply", "arguments": map[string]any{"channel_id": "123", "text": "hello", "message_id": "456"},
	})
	if result.Error != nil {
		t.Fatalf("RPC error = %+v", result.Error)
	}
	if sender.channelID != "123" || sender.message == nil || sender.message.Content != "hello" {
		t.Fatalf("send = channel %q message %+v", sender.channelID, sender.message)
	}
	if sender.message.Reference == nil || sender.message.Reference.MessageID != "456" {
		t.Fatalf("reference = %+v", sender.message.Reference)
	}
	raw, _ := json.Marshal(result.Result)
	if !strings.Contains(string(raw), "sent (1 message(s); ids: 789)") ||
		!strings.Contains(string(raw), `"message_id":"789"`) ||
		!strings.Contains(string(raw), `"message_ids":["789"]`) {
		t.Fatalf("result = %s", raw)
	}
}

func TestDiscordMCPReplyGuardsScopeAndSurfacesErrors(t *testing.T) {
	tests := []struct {
		name    string
		sender  *fakeDiscordSender
		channel string
		want    string
	}{
		{name: "out of scope", sender: &fakeDiscordSender{}, channel: "999", want: "not in scope"},
		{name: "Discord error", sender: &fakeDiscordSender{err: fmt.Errorf("upstream failed")}, channel: "123", want: "Discord rejected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &discordMCPServer{sender: tc.sender, allowedChannels: map[string]bool{"123": true}}
			result := discordRPC(t, s, "tools/call", map[string]any{
				"name": "reply", "arguments": map[string]any{"channel_id": tc.channel, "text": "hello"},
			})
			raw, _ := json.Marshal(result.Result)
			if !strings.Contains(string(raw), tc.want) || !strings.Contains(string(raw), `"isError":true`) {
				t.Fatalf("result = %s, want error containing %q", raw, tc.want)
			}
		})
	}
}

func TestDiscordMCPNotificationHasNoBody(t *testing.T) {
	s := &discordMCPServer{sender: &fakeDiscordSender{}}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	resp := httptest.NewRecorder()
	s.handle(resp, req)
	if resp.Code != http.StatusAccepted || resp.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestDiscordMCPEditAndReact(t *testing.T) {
	sender := &fakeDiscordSender{}
	s := &discordMCPServer{sender: sender, allowedChannels: map[string]bool{"123": true}}
	edit := discordRPC(t, s, "tools/call", map[string]any{
		"name": "edit_message", "arguments": map[string]any{"channel_id": "123", "message_id": "456", "text": "updated"},
	})
	raw, _ := json.Marshal(edit.Result)
	if sender.edited == nil || sender.edited.ID != "456" || sender.edited.Content == nil || *sender.edited.Content != "updated" ||
		!strings.Contains(string(raw), `"message_id":"456"`) {
		t.Fatalf("edited=%+v result=%s", sender.edited, raw)
	}

	discordRPC(t, s, "tools/call", map[string]any{
		"name": "react", "arguments": map[string]any{"channel_id": "123", "message_id": "456", "emoji": "👍"},
	})
	discordRPC(t, s, "tools/call", map[string]any{
		"name": "react", "arguments": map[string]any{"channel_id": "123", "message_id": "456", "emoji": "👍", "remove": true},
	})
	if got := sender.added[len(sender.added)-1]; got != "123/456/👍" {
		t.Fatalf("added = %q", got)
	}
	if got := sender.removed[len(sender.removed)-1]; got != "123/456/👍/@me" {
		t.Fatalf("removed = %q", got)
	}
}

func TestDiscordMCPEditAndReactEnforceBounds(t *testing.T) {
	s := &discordMCPServer{sender: &fakeDiscordSender{}, allowedChannels: map[string]bool{"123": true}}
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "edit outside channel", args: map[string]any{"name": "edit_message", "arguments": map[string]any{"channel_id": "999", "message_id": "1", "text": "x"}}, want: "not in scope"},
		{name: "edit too long", args: map[string]any{"name": "edit_message", "arguments": map[string]any{"channel_id": "123", "message_id": "1", "text": strings.Repeat("x", 2001)}}, want: "Discord allows 2000"},
		{name: "reaction outside channel", args: map[string]any{"name": "react", "arguments": map[string]any{"channel_id": "999", "message_id": "1", "emoji": "👍"}}, want: "not in scope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := discordRPC(t, s, "tools/call", tc.args)
			raw, _ := json.Marshal(resp.Result)
			if !strings.Contains(string(raw), tc.want) || !strings.Contains(string(raw), `"isError":true`) {
				t.Fatalf("result=%s want=%q", raw, tc.want)
			}
		})
	}
}

var _ discordSender = (*fakeDiscordSender)(nil)
