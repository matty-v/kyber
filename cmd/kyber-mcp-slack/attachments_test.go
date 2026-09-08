package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func useSlackPersistRoot(t *testing.T) string {
	t.Helper()
	old := slackPersistRoot
	root := t.TempDir()
	slackPersistRoot = root
	t.Cleanup(func() { slackPersistRoot = old })
	return root
}

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
	if _, _, err := openSlackOutboundFile(path); err == nil || !strings.Contains(err.Error(), "outside /persist") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenSlackOutboundFileRejectsEscapingSymlink(t *testing.T) {
	root := useSlackPersistRoot(t)
	dir, err := os.MkdirTemp(root, "slack-upload-link-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	link := filepath.Join(dir, "escape")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openSlackOutboundFile(link); err == nil {
		t.Fatal("escaping symlink was accepted")
	}
}

func TestDownloadSlackFileScopesHostAndAddsAuth(t *testing.T) {
	if allowedSlackFileURL("https://example.com/file") {
		t.Fatal("non-Slack host was allowed")
	}
	root := useSlackPersistRoot(t)
	dir, err := os.MkdirTemp(root, "slack-download-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
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

func TestDownloadSlackFileRejectsSymlinkedDirectoryOutsidePersist(t *testing.T) {
	root := useSlackPersistRoot(t)
	link := filepath.Join(root, "slack-download-link-test")
	_ = os.Remove(link)
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(link)
	_, err := downloadSlackFile(t.Context(), http.DefaultClient, "secret", observedSlackFile{ID: "F1", URL: "https://files.slack.com/file"}, link)
	if err == nil || !strings.Contains(err.Error(), "outside /persist") {
		t.Fatalf("error = %v", err)
	}
}

func TestDownloadSlackFileRejectsUnsafeFileID(t *testing.T) {
	root := useSlackPersistRoot(t)
	_, err := downloadSlackFile(t.Context(), http.DefaultClient, "secret", observedSlackFile{ID: "../../escape", URL: "https://files.slack.com/file"}, filepath.Join(root, "slack-attachments"))
	if err == nil || !strings.Contains(err.Error(), "file ID") {
		t.Fatalf("error = %v", err)
	}
}
