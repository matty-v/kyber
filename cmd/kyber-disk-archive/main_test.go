package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/diskarchive"
)

func TestExportStreamsVerifiableArchive(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "agentroot/home/kyber"), 0o755)
	os.WriteFile(filepath.Join(root, "agentroot/home/kyber/notes.md"), []byte("hello"), 0o644)

	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("KYBER_ARCHIVE_UPLOAD_URL", srv.URL)
	t.Setenv("KYBER_ARCHIVE_TOKEN", "tok")
	t.Setenv("KYBER_ARCHIVE_DESCRIPTION", `{"source":{"agent":"han","runtime":"codex"},"mounts":[{"path":"/persist","kind":"pvc","archived":true,"reason":"disk"}]}`)

	if err := runExport(context.Background(), root); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	m, err := diskarchive.Verify(bytes.NewReader(got), int64(len(got)), diskarchive.Limits{})
	if err != nil {
		t.Fatalf("uploaded archive does not verify: %v", err)
	}
	if m.Source.Agent != "han" || len(m.Mounts) != 1 || m.Totals.Files != 1 {
		t.Errorf("manifest = %+v", m)
	}
}

func TestExportReportsRejectionAndWalkFailure(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("KYBER_ARCHIVE_UPLOAD_URL", srv.URL)
	t.Setenv("KYBER_ARCHIVE_TOKEN", "tok")
	if err := runExport(context.Background(), root); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("rejected upload = %v, want HTTP 403", err)
	}

	os.WriteFile(filepath.Join(root, "big"), bytes.Repeat([]byte("x"), 100), 0o644)
	t.Setenv("KYBER_ARCHIVE_MAX_BYTES", "10")
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			return // the walk aborted the stream
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	t.Setenv("KYBER_ARCHIVE_UPLOAD_URL", ok.URL)
	if err := runExport(context.Background(), root); err == nil || !strings.Contains(err.Error(), "archiving") {
		t.Errorf("oversized walk = %v, want an archiving error", err)
	}
}
