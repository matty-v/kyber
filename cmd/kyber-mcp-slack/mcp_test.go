package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestSlackMCPReplyUploadsPersistFile(t *testing.T) {
	dir, err := os.MkdirTemp("/persist", "slack-upload-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		body := `{"ok":true}`
		switch r.URL.Path {
		case "/api/files.getUploadURLExternal":
			body = `{"ok":true,"upload_url":"https://files.slack.com/upload/v1/test","file_id":"F1"}`
		case "/upload/v1/test":
			if r.Header.Get("Authorization") != "" {
				t.Fatal("bot token leaked to signed upload URL")
			}
			if _, err := io.ReadAll(r.Body); err != nil {
				t.Fatal(err)
			}
		case "/api/files.completeUploadExternal":
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	original := slackAPIBaseURL
	slackAPIBaseURL = "https://slack.com/api/"
	defer func() { slackAPIBaseURL = original }()
	s := newMCPServer(config{botToken: "secret", channels: map[string]bool{"C123": true}}, client)
	args, _ := json.Marshal(map[string]any{"name": "reply", "arguments": map[string]any{"channel_id": "C123", "text": "attached", "files": []string{path}}})
	got := s.call(t.Context(), args)
	if got.IsError || len(calls) != 3 {
		t.Fatalf("call = %+v, paths = %v", got, calls)
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

func TestSlackButtonBlocks(t *testing.T) {
	blocks := slackButtonBlocks("Choose", []any{map[string]any{"text": "Approve", "value": "yes", "action_id": "approve"}})
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v", blocks)
	}
	elements := blocks[1]["elements"].([]map[string]any)
	if elements[0]["action_id"] != "approve" || elements[0]["value"] != "yes" {
		t.Fatalf("button = %+v", elements[0])
	}
}
