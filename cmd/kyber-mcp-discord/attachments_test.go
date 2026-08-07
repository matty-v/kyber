package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestInboundAttachmentsAndBoundedScope(t *testing.T) {
	store := newAttachmentStore(2)
	items := []*discordgo.MessageAttachment{
		{ID: "a", URL: "https://cdn.discordapp.com/attachments/a", Filename: "a.txt", ContentType: "text/plain", Size: 10},
		{ID: "b", URL: "https://cdn.discordapp.com/attachments/b", Filename: "b.png", Width: 12, Height: 13},
		{ID: "c", URL: "https://cdn.discordapp.com/attachments/c", Filename: "c.txt"},
	}
	metadata := inboundAttachments(items[:2])
	if len(metadata) != 2 || metadata[0].ID != "a" || metadata[1].Width != 12 {
		t.Fatalf("metadata = %+v", metadata)
	}
	store.observe(items)
	if _, ok := store.get("a"); ok {
		t.Fatal("oldest attachment was not evicted")
	}
	if _, ok := store.get("b"); !ok {
		t.Fatal("recent attachment missing")
	}
}

func TestDownloadDiscordAttachment(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("hello")), Header: http.Header{}}, nil
	})}
	dir := t.TempDir()
	path, err := downloadDiscordAttachment(context.Background(), client, observedAttachment{
		ID: "123", URL: "https://cdn.discordapp.com/attachments/123/file.txt", Filename: "../file.txt", Size: 5,
	}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir || filepath.Base(path) != "123-file.txt" {
		t.Fatalf("path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hello" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 644", info.Mode().Perm())
	}
}

func TestDownloadDiscordAttachmentRejectsUntrustedURLAndOversize(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("rejected attachment reached the network")
		return nil, nil
	})}
	tests := []observedAttachment{
		{ID: "1", URL: "https://example.com/file", Filename: "x"},
		{ID: "2", URL: "https://cdn.discordapp.com/attachments/2/x", Filename: "x", Size: int(maxDiscordAttachmentBytes + 1)},
	}
	for _, item := range tests {
		if _, err := downloadDiscordAttachment(context.Background(), client, item, t.TempDir()); err == nil {
			t.Errorf("item %+v was accepted", item)
		}
	}
}

func TestDiscordAttachmentClientRejectsRedirectOutsideCDN(t *testing.T) {
	client := newDiscordAttachmentClient(time.Second)
	req, err := http.NewRequest(http.MethodGet, "https://example.com/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(req, nil); err == nil {
		t.Fatal("redirect outside Discord CDN was accepted")
	}
}

func TestValidateDiscordOutboundFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := validateDiscordOutboundFile(path, []string{root}); err != nil || got != want {
		t.Fatalf("got=%q want=%q err=%v", got, want, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDiscordOutboundFile(outside, []string{root}); err == nil {
		t.Fatal("outside file accepted")
	}
}

func TestSendDiscordMessageAttachesFilesToFirstChunk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender := &fakeDiscordSender{}
	if _, err := sendDiscordMessage(sender, "123", strings.Repeat("word ", 1000), "", []string{path}); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) < 2 || len(sender.messages[0].Files) != 1 || sender.messages[0].Files[0].Name != "report.txt" {
		t.Fatalf("messages = %+v", sender.messages)
	}
	for _, message := range sender.messages[1:] {
		if len(message.Files) != 0 {
			t.Fatal("attachment repeated on a later chunk")
		}
	}
}

func TestDiscordMCPDownloadAttachmentEnforcesObservedScope(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("payload")), Header: http.Header{}}, nil
	})}
	store := newAttachmentStore(256)
	store.observe([]*discordgo.MessageAttachment{{
		ID: "observed", URL: "https://cdn.discordapp.com/attachments/observed/file.txt", Filename: "file.txt", Size: 7,
	}})
	s := &discordMCPServer{sender: &fakeDiscordSender{}, attachments: store, client: client, downloadDir: t.TempDir()}

	accepted := discordRPC(t, s, "tools/call", map[string]any{
		"name": "download_attachment", "arguments": map[string]any{"attachment_id": "observed"},
	})
	raw, _ := json.Marshal(accepted.Result)
	if !strings.Contains(string(raw), "downloaded to") || strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("accepted result = %s", raw)
	}

	rejected := discordRPC(t, s, "tools/call", map[string]any{
		"name": "download_attachment", "arguments": map[string]any{"attachment_id": "unknown"},
	})
	raw, _ = json.Marshal(rejected.Result)
	if !strings.Contains(string(raw), "not in scope") || !strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("rejected result = %s", raw)
	}
}
