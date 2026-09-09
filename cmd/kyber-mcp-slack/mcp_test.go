package main

import (
	"encoding/json"
	"fmt"
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
	root := useSlackPersistRoot(t)
	dir, err := os.MkdirTemp(root, "slack-upload-test-")
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
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q, want application/json", got)
			}
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request["filename"] != "report.txt" || request["length"] != float64(5) {
				t.Fatalf("upload reservation = %#v", request)
			}
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

func TestSlackAPIErrorIncludesBoundedResponseMetadataMessages(t *testing.T) {
	long := strings.Repeat("x", 300)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "invalid_arguments",
			"response_metadata": map[string]any{"messages": []any{
				"  [ERROR] missing required field: length\n",
				long,
				"third",
				"fourth is omitted",
			}},
		})
	}))
	defer api.Close()
	original := slackAPIBaseURL
	slackAPIBaseURL = api.URL + "/"
	defer func() { slackAPIBaseURL = original }()

	s := newMCPServer(config{botToken: "secret"}, api.Client())
	_, err := s.api(t.Context(), "files.getUploadURLExternal", map[string]any{"filename": "a.txt", "length": 1})
	if err == nil {
		t.Fatal("expected Slack API error")
	}
	got := err.Error()
	if !strings.Contains(got, "invalid_arguments: [ERROR] missing required field: length") {
		t.Fatalf("error = %q", got)
	}
	if strings.Contains(got, "fourth") || !strings.Contains(got, strings.Repeat("x", 256)+"…") {
		t.Fatalf("error details were not bounded: %q", got)
	}
}

func TestSlackUploadAggregateLimitIsCheckedBeforeNetwork(t *testing.T) {
	root := useSlackPersistRoot(t)
	dir, err := os.MkdirTemp(root, "slack-upload-limit-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("part-%d", i))
		file, err := os.Create(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(8 << 20); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("network called before aggregate validation")
		return nil, nil
	})}
	s := newMCPServer(config{channels: map[string]bool{"C": true}}, client)
	got := s.uploadFiles(t.Context(), "C", "", "files", paths)
	if !got.IsError || !strings.Contains(got.Content[0]["text"], "aggregate") {
		t.Fatalf("result = %+v", got)
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

func TestSplitSlackTextPreservesAllContentWithinLimit(t *testing.T) {
	input := strings.Repeat("word ", 1200)
	parts := splitSlackText(input)
	if len(parts) < 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	for _, part := range parts {
		if len([]rune(part)) > slackMessageLimit {
			t.Fatalf("chunk has %d runes", len([]rune(part)))
		}
	}
	if strings.Join(parts, " ") != strings.TrimSpace(input) {
		t.Fatal("split changed content")
	}
}

func TestSlackButtonCallbacksAreOpaque(t *testing.T) {
	registry := newSlackCallbackRegistry()
	blocks, tokens, err := registry.register("C123", []any{map[string]any{"text": "Approve", "value": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || len(tokens) != 1 {
		t.Fatalf("blocks = %+v", blocks)
	}
	elements := blocks[0]["elements"].([]map[string]any)
	if elements[0]["value"] == "yes" || elements[0]["value"] != tokens[0] {
		t.Fatalf("button = %+v", elements[0])
	}
}
