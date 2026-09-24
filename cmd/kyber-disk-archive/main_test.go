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

	m := exportWith(t, root, `{"source":{"agent":"han","runtime":"codex","config":{"version":2,"startupPrompt":"`+strings.Repeat("x", 200<<10)+`"}},"mounts":[{"path":"/persist","kind":"pvc","archived":true,"reason":"disk"}]}`)
	// The description is larger than any one environment variable may be.
	if m.Source.Agent != "han" || len(m.Mounts) != 1 || m.Totals.Files != 1 || len(m.Source.Config.StartupPrompt) != 200<<10 {
		t.Errorf("manifest = %+v", m.Totals)
	}
}

// exportWith runs an export against a fake control plane serving desc, and
// returns the verified manifest of what was uploaded.
func exportWith(t *testing.T, root, desc string) *diskarchive.Manifest {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/description":
			io.WriteString(w, desc)
		case r.Method == http.MethodPut && r.URL.Path == "/upload":
			got, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	t.Setenv("KYBER_ARCHIVE_UPLOAD_URL", srv.URL+"/upload")
	t.Setenv("KYBER_ARCHIVE_DESCRIPTION_URL", srv.URL+"/description")
	t.Setenv("KYBER_ARCHIVE_TOKEN", "tok")
	if err := runExport(context.Background(), root); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	m, err := diskarchive.Verify(bytes.NewReader(got), int64(len(got)), diskarchive.Limits{})
	if err != nil {
		t.Fatalf("uploaded archive does not verify: %v", err)
	}
	return m
}

func TestExportMarksImageFiles(t *testing.T) {
	root := t.TempDir()
	write := func(p, body string, mtime time.Time) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
		os.Chtimes(full, mtime, mtime)
	}
	shipped := time.Unix(1770985020, 0)
	write("agentroot/etc/cron.d/e2scrub_all", strings.Repeat("c", 188), shipped)
	write("agentroot/usr/lib/node_modules/npm/.npmrc", "", shipped)
	write("agentroot/usr/lib/node_modules/x/.npmrc", "registry=a\n", shipped)
	write("agentroot/etc/cron.d/agent-job", "* * * * * x\n", shipped)
	// Same size as the image's copy, but written by the agent later.
	write("agentroot/usr/lib/node_modules/y/.npmrc", "//r/:_authToken=t", shipped.Add(time.Hour))
	write(diskarchive.ImageManifestPath, "#kyber-rootfs-manifest v2\n"+
		"f\t188\t1770985020.0000000000\t644\t./etc/cron.d/e2scrub_all\n"+
		"f\t0\t1770985020.0000000000\t644\t./usr/lib/node_modules/npm/.npmrc\n"+
		"f\t11\t1770985020.0000000000\t644\t./usr/lib/node_modules/x/.npmrc\n"+
		"f\t17\t1770985020.0000000000\t644\t./usr/lib/node_modules/y/.npmrc\n", shipped)

	m := exportWith(t, root, `{"source":{"agent":"han"}}`)
	if got := diskarchive.SensitivePaths(m); len(got) != 1 || got[0] != "agentroot/usr/lib/node_modules/y/.npmrc" {
		t.Errorf("sensitive = %v, want only the agent-changed .npmrc", got)
	}
	if got := diskarchive.CronPaths(m); len(got) != 1 || got[0] != "agentroot/etc/cron.d/agent-job" {
		t.Errorf("cron = %v, want only the agent's crontab", got)
	}
}

func TestExportReportsRejectionAndWalkFailure(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("KYBER_ARCHIVE_UPLOAD_URL", srv.URL)
	t.Setenv("KYBER_ARCHIVE_DESCRIPTION_URL", srv.URL)
	t.Setenv("KYBER_ARCHIVE_TOKEN", "tok")
	if err := runExport(context.Background(), root); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("rejected upload = %v, want HTTP 403", err)
	}

	os.WriteFile(filepath.Join(root, "big"), bytes.Repeat([]byte("x"), 100), 0o644)
	t.Setenv("KYBER_ARCHIVE_MAX_BYTES", "10")
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, "{}")
			return
		}
		if _, err := io.ReadAll(r.Body); err != nil {
			return // the walk aborted the stream
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	t.Setenv("KYBER_ARCHIVE_UPLOAD_URL", ok.URL)
	t.Setenv("KYBER_ARCHIVE_DESCRIPTION_URL", ok.URL)
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

	// Opting in restores them, except the harness login, which never travels.
	t.Setenv("KYBER_ARCHIVE_SKIP_CREDENTIALS", "false")
	t.Setenv("KYBER_ARCHIVE_SKIP_CRONTABS", "false")
	t.Setenv("KYBER_ARCHIVE_LOGIN_FILES", ".claude/.credentials.json,.codex/auth.json")
	all := t.TempDir()
	if err := runRestore(context.Background(), all); err != nil {
		t.Fatalf("runRestore keeping everything: %v", err)
	}
	for _, p := range []string{"agentroot/var/spool/cron/crontabs/kyber", "agentroot/home/kyber/.ssh/id_ed25519"} {
		if _, err := os.Stat(filepath.Join(all, p)); err != nil {
			t.Errorf("opted-in %s missing: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(all, "agentroot/home/kyber/.claude/.credentials.json")); !os.IsNotExist(err) {
		t.Errorf("harness login restored with keepCredentialFiles: %v", err)
	}
}
