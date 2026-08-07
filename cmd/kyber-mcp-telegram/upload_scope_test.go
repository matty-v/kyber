package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateOutboundFileAllowsRegularFileUnderRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "report.txt")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := validateOutboundFile(path, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("validated path = %q, want %q", got, want)
	}
}

func TestValidateOutboundFileRejectsOutsideRootAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "looks-safe.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{outside, link} {
		if _, err := validateOutboundFile(path, []string{root}); err == nil {
			t.Errorf("validateOutboundFile(%q) accepted a path outside the root", path)
		}
	}
}

func TestValidateOutboundFileRejectsOversizedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxUploadBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	if _, err := validateOutboundFile(path, []string{root}); err == nil || !strings.Contains(err.Error(), "upload limit") {
		t.Fatalf("oversized file error = %v", err)
	}
}

func TestCallAPIMultipartRejectsOversizedFileBeforeTelegram(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "large.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxUploadBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	cfg := &config{botToken: "TOK", botAPIBaseURL: server.URL}
	_, err = callAPIMultipart(context.Background(), cfg, server.Client(), "sendDocument", "document", path,
		url.Values{"chat_id": {"42"}})
	if err == nil || !strings.Contains(err.Error(), "upload limit") {
		t.Fatalf("oversized upload error = %v", err)
	}
	if called {
		t.Fatal("oversized upload reached Telegram")
	}
}
