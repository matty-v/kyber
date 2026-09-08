package main

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSlackAttachmentStoreIsBounded(t *testing.T) {
	store := newSlackAttachmentStore(1)
	store.observe([]slackFile{{ID: "old", URL: "https://files.slack.com/old"}})
	store.observe([]slackFile{{ID: "new", URL: "https://files.slack.com/new"}})
	if _, ok := store.get("old"); ok {
		t.Fatal("old file remained in bounded store")
	}
	if _, ok := store.get("new"); !ok {
		t.Fatal("new file was not observed")
	}
}

func TestValidateSlackOutboundFileRejectsOutsidePersist(t *testing.T) {
	path := t.TempDir() + "/outside.txt"
	if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateSlackOutboundFile(path); err == nil || !strings.Contains(err.Error(), "outside /persist") {
		t.Fatalf("error = %v", err)
	}
}

func TestDownloadSlackFileScopesHostAndAddsAuth(t *testing.T) {
	if allowedSlackFileURL("https://example.com/file") {
		t.Fatal("non-Slack host was allowed")
	}
	dir := t.TempDir()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("hello")), Header: make(http.Header)}, nil
	})}
	path, err := downloadSlackFile(t.Context(), client, "secret", observedSlackFile{ID: "F1", Name: "../note.txt", URL: "https://files.slack.com/files-pri/T-F/download/note.txt", Size: 5}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/F1-note.txt") {
		t.Fatalf("path = %q", path)
	}
}

func TestInboundSlackFilesDoNotExposePrivateURL(t *testing.T) {
	got := inboundSlackFiles([]slackFile{{ID: "F1", Name: "note.txt", URL: "https://files.slack.com/private"}})
	if len(got) != 1 || got[0].ID != "F1" {
		t.Fatalf("files = %+v", got)
	}
}
