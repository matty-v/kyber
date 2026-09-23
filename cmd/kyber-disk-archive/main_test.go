package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestVerifyReadsByRangeAndPostsSummary(t *testing.T) {
	root := t.TempDir()
	noise := make([]byte, 9<<20) // incompressible, so the archive spans several read blocks
	rand.Read(noise)
	os.WriteFile(filepath.Join(root, "f"), noise, 0o644)
	var buf bytes.Buffer
	if _, err := diskarchive.Write(context.Background(), root, &buf, diskarchive.WriteOptions{Source: diskarchive.Source{Agent: "han"}}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	var posted []byte
	ranges := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/archive":
			if r.Header.Get("Range") != "" {
				ranges++
			}
			http.ServeContent(w, r, "a.zip", time.Time{}, bytes.NewReader(data))
		case "/summary":
			posted, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	t.Setenv("KYBER_ARCHIVE_SOURCE_URL", srv.URL+"/archive")
	t.Setenv("KYBER_ARCHIVE_SUMMARY_URL", srv.URL+"/summary")
	t.Setenv("KYBER_ARCHIVE_TOKEN", "tok")
	if err := runVerify(context.Background()); err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	if ranges < 2 || !strings.Contains(string(posted), `"agent":"han"`) || strings.Contains(string(posted), `"entries":[`) {
		t.Errorf("ranges=%d posted=%s", ranges, posted)
	}

	// A corrupt archive fails and reports nothing.
	data = append([]byte(nil), data...)
	data[len(data)/3] ^= 0xff
	posted = nil
	if err := runVerify(context.Background()); err == nil || posted != nil {
		t.Errorf("corrupt archive: err=%v posted=%s", err, posted)
	}
}

func TestRestoreExtractsAndVerifiesWithSkips(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "agentroot/home/kyber/.claude"), 0o700)
	os.MkdirAll(filepath.Join(src, "agentroot/home/kyber/.ssh"), 0o700)
	os.MkdirAll(filepath.Join(src, "agentroot/var/spool/cron/crontabs"), 0o700)
	os.WriteFile(filepath.Join(src, "agentroot/home/kyber/.claude/.credentials.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(src, "agentroot/home/kyber/.ssh/id_ed25519"), []byte("key"), 0o600)
	os.WriteFile(filepath.Join(src, "agentroot/var/spool/cron/crontabs/kyber"), []byte("* * * * * work\n"), 0o600)
	os.WriteFile(filepath.Join(src, "agentroot/home/kyber/work.txt"), []byte("keep me"), 0o644)
	os.Symlink("home/kyber", filepath.Join(src, "agentroot/me"))
	var buf bytes.Buffer
	if _, err := diskarchive.Write(context.Background(), src, &buf, diskarchive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, "a.zip", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	t.Setenv("KYBER_ARCHIVE_SOURCE_URL", srv.URL)
	t.Setenv("KYBER_ARCHIVE_TOKEN", "tok")

	dst := t.TempDir()
	if err := runRestore(context.Background(), dst); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "agentroot/me/work.txt")); err != nil || string(b) != "keep me" {
		t.Errorf("restored work = %q, %v", b, err)
	}
	for _, p := range []string{"agentroot/home/kyber/.claude/.credentials.json", "agentroot/home/kyber/.ssh/id_ed25519", "agentroot/var/spool/cron/crontabs/kyber"} {
		if _, err := os.Stat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("%s was restored by default: %v", p, err)
		}
	}
	// A second restore into the now non-empty volume is refused.
	if err := runRestore(context.Background(), dst); err == nil {
		t.Error("restore into a non-empty volume succeeded")
	}

	// Opting in restores them.
	t.Setenv("KYBER_ARCHIVE_SKIP_CREDENTIALS", "false")
	t.Setenv("KYBER_ARCHIVE_SKIP_CRONTABS", "false")
	all := t.TempDir()
	if err := runRestore(context.Background(), all); err != nil {
		t.Fatalf("runRestore keeping everything: %v", err)
	}
	if _, err := os.Stat(filepath.Join(all, "agentroot/var/spool/cron/crontabs/kyber")); err != nil {
		t.Errorf("opted-in crontab missing: %v", err)
	}
}
