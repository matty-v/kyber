package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendFileUsesNativeTelegramMethod(t *testing.T) {
	tests := map[string]string{
		"image.png": "sendPhoto", "clip.gif": "sendAnimation", "movie.mp4": "sendVideo",
		"song.mp3": "sendAudio", "note.ogg": "sendVoice", "report.pdf": "sendDocument",
	}
	for name, wantMethod := range tests {
		t.Run(name, func(t *testing.T) {
			var gotMethod string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				gotMethod = parts[len(parts)-1]
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config{botToken: "t", botAPIBaseURL: server.URL}
			if _, err := sendFile(context.Background(), cfg, server.Client(), "42", path, "", ""); err != nil {
				t.Fatal(err)
			}
			if gotMethod != wantMethod {
				t.Fatalf("method=%q want %q", gotMethod, wantMethod)
			}
		})
	}
}

func TestDownloadFileRejectsOversizedBodyWithoutMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getFile") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"uploads/large.bin"}}`))
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxDownloadBytes+1))
	}))
	defer server.Close()
	destDir := t.TempDir()
	cfg := &config{botToken: "t", botAPIBaseURL: server.URL}
	if _, err := downloadFile(context.Background(), cfg, server.Client(), "file-id", destDir); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized download error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "large.bin")); !os.IsNotExist(err) {
		t.Fatalf("partial oversized download was retained: %v", err)
	}
}

func TestDownloadFileReplacesDestinationSymlinkWithoutFollowingIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getFile") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"uploads/photo.jpg"}}`))
			return
		}
		_, _ = w.Write([]byte("downloaded"))
	}))
	defer server.Close()
	destDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "root-owned.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(destDir, "photo.jpg")
	if err := os.Symlink(outside, dest); err != nil {
		t.Fatal(err)
	}
	cfg := &config{botToken: "t", botAPIBaseURL: server.URL}
	got, err := downloadFile(context.Background(), cfg, server.Client(), "file-id", destDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dest {
		t.Fatalf("download path=%q want %q", got, dest)
	}
	outsideBody, _ := os.ReadFile(outside)
	destBody, _ := os.ReadFile(dest)
	if string(outsideBody) != "keep" || string(destBody) != "downloaded" {
		t.Fatalf("outside=%q destination=%q", outsideBody, destBody)
	}
}

func TestSendMediaGroupUsesNativeAlbum(t *testing.T) {
	var gotMethod string
	var gotMedia string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		gotMethod = parts[len(parts)-1]
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			t.Error(err)
		}
		gotMedia = r.FormValue("media")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "one.jpg"), filepath.Join(dir, "two.mp4")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config{botToken: "t", botAPIBaseURL: server.URL}
	if err := sendMediaGroup(context.Background(), cfg, server.Client(), "42", paths); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "sendMediaGroup" || !strings.Contains(gotMedia, `"type":"photo"`) || !strings.Contains(gotMedia, `"type":"video"`) {
		t.Fatalf("method=%q media=%q", gotMethod, gotMedia)
	}
}
