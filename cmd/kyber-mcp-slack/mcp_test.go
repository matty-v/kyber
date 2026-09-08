package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSlackMCPChannelAllowlist(t *testing.T) {
	s := newMCPServer(config{channels: map[string]bool{"C123": true}}, nil)
	if !s.channelAllowed("C123") {
		t.Fatal("allowlisted channel was rejected")
	}
	if s.channelAllowed("C999") {
		t.Fatal("unallowlisted channel was accepted")
	}
	if newMCPServer(config{}, nil).channelAllowed("C123") {
		t.Fatal("empty allowlist must fail closed")
	}
}

func TestSlackMCPAdvertisesAndCallsRichTools(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.update" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.2"})
	}))
	defer api.Close()
	client := api.Client()
	s := newMCPServer(config{botToken: "test", channels: map[string]bool{"C123": true}}, client)
	// Redirect Slack requests into the fake API.
	original := slackAPIBaseURL
	slackAPIBaseURL = api.URL + "/"
	defer func() { slackAPIBaseURL = original }()
	args, _ := json.Marshal(map[string]any{"name": "edit_message", "arguments": map[string]any{"channel_id": "C123", "message_id": "1.1", "text": "fixed"}})
	got := s.call(t.Context(), args)
	if got.IsError || !strings.Contains(got.Content[0]["text"], "1.2") {
		t.Fatalf("call = %+v", got)
	}
}

func TestSlackMCPDownloadRejectsUnobservedFile(t *testing.T) {
	s := newMCPServer(config{attachments: newSlackAttachmentStore(10)}, http.DefaultClient)
	args, _ := json.Marshal(map[string]any{"name": "download_attachment", "arguments": map[string]any{"file_id": "unknown"}})
	got := s.call(t.Context(), args)
	if !got.IsError || !strings.Contains(got.Content[0]["text"], "not in scope") {
		t.Fatalf("call = %+v", got)
	}
}
