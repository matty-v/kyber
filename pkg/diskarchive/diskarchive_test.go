package diskarchive

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildTree creates a representative agent volume: nested and hidden files,
// an empty file, a setgid directory, relative and absolute symlinks, a
// dangling symlink, and a hard link.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "agentroot/home/kyber/.claude/projects"), 0o755))
	must(os.MkdirAll(filepath.Join(root, "agentroot/home/kyber/dev/identity/skills/x"), 0o750))
	must(os.WriteFile(filepath.Join(root, "agentroot/home/kyber/.claude/.credentials.json"), []byte(`{"secret":"x"}`), 0o600))
	must(os.WriteFile(filepath.Join(root, "agentroot/home/kyber/dev/identity/skills/x/SKILL.md"), []byte("# skill\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "agentroot/home/kyber/.claude/projects/s.jsonl"), bytes.Repeat([]byte("line\n"), 50000), 0o644))
	must(os.WriteFile(filepath.Join(root, "agentroot/empty"), nil, 0o640))
	must(os.Mkdir(filepath.Join(root, "shared"), 0o775))
	must(os.Chmod(filepath.Join(root, "shared"), 0o775|os.ModeSetgid))
	must(os.Symlink("home/kyber", filepath.Join(root, "agentroot/me")))
	must(os.Symlink("/usr/bin/python3", filepath.Join(root, "agentroot/python")))
	must(os.Symlink("missing", filepath.Join(root, "agentroot/dangling")))
	must(os.Link(filepath.Join(root, "agentroot/empty"), filepath.Join(root, "agentroot/hardlink")))
	must(os.Mkdir(filepath.Join(root, "lost+found"), 0o700))
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	must(os.Chtimes(filepath.Join(root, "agentroot/home/kyber/dev/identity/skills/x/SKILL.md"), old, old))
	return root
}

func export(t *testing.T, root string) ([]byte, *Manifest) {
	t.Helper()
	var buf bytes.Buffer
	m, err := Write(context.Background(), root, &buf, WriteOptions{
		Scan:   ScanOptions{Exclude: DefaultExclusions()},
		Source: Source{Agent: "han", Runtime: "claude-code"},
		Mounts: []Mount{{Path: "/persist", Kind: "persistentVolumeClaim", Archived: true, Reason: "agent disk"}},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes(), m
}

func TestRoundTripPreservesTreeAndMetadata(t *testing.T) {
	src := buildTree(t)
	data, m := export(t, src)

	if m.FormatVersion != FormatVersion || m.Source.Agent != "han" {
		t.Fatalf("manifest identity = %q/%q", m.FormatVersion, m.Source.Agent)
	}
	if len(m.Excluded) != 1 || m.Excluded[0].Path != "lost+found" {
		t.Errorf("excluded = %+v, want lost+found only", m.Excluded)
	}
	byPath := map[string]Entry{}
	for _, e := range m.Entries {
		byPath[e.Path] = e
	}
	if e := byPath["agentroot/python"]; e.Type != EntrySymlink || e.Target != "/usr/bin/python3" {
		t.Errorf("absolute symlink recorded as %+v", e)
	}
	if e := byPath["shared"]; e.Mode != 0o2775 {
		t.Errorf("setgid dir mode = %o, want 2775", e.Mode)
	}
	if e := byPath["agentroot/hardlink"]; e.Type != EntryFile {
		t.Errorf("hard link recorded as %q, want an independent file", e.Type)
	}

	verified, err := Verify(bytes.NewReader(data), int64(len(data)), Limits{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified.Totals != m.Totals {
		t.Errorf("verified totals %+v != written %+v", verified.Totals, m.Totals)
	}

	dst := t.TempDir()
	if err := os.Mkdir(filepath.Join(dst, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	allowed := []string{"lost+found"}
	if _, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data)), dst, ExtractOptions{Allowed: allowed}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := VerifyRestore(context.Background(), dst, m, allowed, nil); err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "agentroot/me/dev/identity/skills/x/SKILL.md"))
	if err != nil || string(got) != "# skill\n" {
		t.Errorf("skill through restored symlink = %q, %v", got, err)
	}
}

func TestSpecialFilesAreListedNotSilentlyDropped(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// Unix socket paths are length-limited; bind in a short temp dir.
	sockDir, err := os.MkdirTemp("", "da")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	ln, err := net.Listen("unix", filepath.Join(sockDir, "s"))
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	defer ln.Close()
	if err := os.Rename(filepath.Join(sockDir, "s"), filepath.Join(root, "sock")); err != nil {
		t.Skipf("moving socket: %v", err)
	}
	_, m := export(t, root)
	reasons := map[string]string{}
	for _, x := range m.Excluded {
		reasons[x.Path] = x.Reason
	}
	for _, p := range []string{"fifo", "sock"} {
		if reasons[p] == "" {
			t.Errorf("%s missing from exclusions: %+v", p, m.Excluded)
		}
	}
}

func TestUnreadableFileFailsExport(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "locked"), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := Write(context.Background(), root, &bytes.Buffer{}, WriteOptions{})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("Write error = %v, want ErrChanged", err)
	}
}

func TestExportLimits(t *testing.T) {
	root := buildTree(t)
	tests := []struct {
		name string
		opts ScanOptions
	}{
		{"entries", ScanOptions{MaxEntries: 3}},
		{"bytes", ScanOptions{MaxBytes: 10}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Write(context.Background(), root, &bytes.Buffer{}, WriteOptions{Scan: tc.opts})
			if !errors.Is(err, ErrLimitExceeded) {
				t.Errorf("Write = %v, want ErrLimitExceeded", err)
			}
		})
	}
}

func TestCanceledExportStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Write(ctx, buildTree(t), &bytes.Buffer{}, WriteOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Write = %v, want context.Canceled", err)
	}
}

// craft builds an archive from a manifest and explicit ZIP entries so tests
// can express archives Write would never produce.
type craftEntry struct {
	name string
	mode fs.FileMode
	body string
}

func craft(t *testing.T, m Manifest, entries []craftEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.body))
	}
	w, _ := zw.Create(ManifestName)
	json.NewEncoder(w).Encode(m)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const sumOfX = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"

func baseManifest(extra ...Entry) Manifest {
	entries := append([]Entry{{Path: ".", Type: EntryDir, Mode: 0o755}}, extra...)
	var tot Totals
	for _, e := range entries {
		tot.Entries++
		switch e.Type {
		case EntryDir:
			tot.Dirs++
		case EntryFile:
			tot.Files++
			tot.Bytes += e.Size
		case EntrySymlink:
			tot.Symlinks++
		}
	}
	return Manifest{FormatVersion: FormatVersion, Entries: entries, Totals: tot}
}

func TestInspectRejectsUnsafeArchives(t *testing.T) {
	root := craftEntry{PayloadPrefix, fs.ModeDir | 0o755, ""}
	fileX := func(p string) Entry { return Entry{Path: p, Type: EntryFile, Size: 1, Mode: 0o644, SHA256: sumOfX} }
	tests := []struct {
		name    string
		m       Manifest
		entries []craftEntry
		wantErr error
	}{
		{"traversal", baseManifest(fileX("../evil")), []craftEntry{root, {"persist/../evil", 0o644, "x"}}, ErrUnsafePath},
		{"absolute", baseManifest(fileX("/etc/passwd")), []craftEntry{root, {"persist//etc/passwd", 0o644, "x"}}, ErrUnsafePath},
		{"backslash traversal", baseManifest(fileX(`a\..\b`)), []craftEntry{root, {`persist/a\..\b`, 0o644, "x"}}, ErrUnsafePath},
		{"no entries", Manifest{FormatVersion: FormatVersion}, nil, ErrInvalidArchive},
		{"unlisted zip entry", baseManifest(), []craftEntry{root, {"persist/extra", 0o644, "x"}}, ErrInvalidArchive},
		{"listed but missing", baseManifest(fileX("gone")), []craftEntry{root}, ErrInvalidArchive},
		{"duplicate zip names", baseManifest(fileX("a")), []craftEntry{root, {"persist/a", 0o644, "x"}, {"persist/a", 0o644, "x"}}, ErrInvalidArchive},
		{"special file", baseManifest(Entry{Path: "p", Type: "fifo"}), []craftEntry{root, {"persist/p", fs.ModeNamedPipe | 0o644, ""}}, ErrInvalidArchive},
		{"file under symlink", baseManifest(
			Entry{Path: "l", Type: EntrySymlink, Target: "/etc", Mode: 0o777}, fileX("l/passwd")),
			[]craftEntry{root, {"persist/l", fs.ModeSymlink | 0o777, "/etc"}, {"persist/l/passwd", 0o644, "x"}}, ErrInvalidArchive},
		{"size mismatch", baseManifest(Entry{Path: "a", Type: EntryFile, Size: 5, Mode: 0o644, SHA256: sumOfX}),
			[]craftEntry{root, {"persist/a", 0o644, "x"}}, ErrInvalidArchive},
		{"bad mode bits", baseManifest(Entry{Path: "a", Type: EntryFile, Size: 1, Mode: 0o170644, SHA256: sumOfX}),
			[]craftEntry{root, {"persist/a", 0o644, "x"}}, ErrInvalidArchive},
		{"unsupported version", func() Manifest { m := baseManifest(); m.FormatVersion = "kyber.io/agent-disk-archive/v9"; return m }(),
			[]craftEntry{root}, ErrUnsupportedVersion},
		{"totals lie", func() Manifest { m := baseManifest(fileX("a")); m.Totals.Bytes = 0; return m }(),
			[]craftEntry{root, {"persist/a", 0o644, "x"}}, ErrInvalidArchive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := craft(t, tc.m, tc.entries)
			_, err := Inspect(bytes.NewReader(data), int64(len(data)), Limits{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Inspect = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestInspectEnforcesDestinationCapacity(t *testing.T) {
	data, _ := export(t, buildTree(t))
	_, err := Inspect(bytes.NewReader(data), int64(len(data)), Limits{MaxBytes: 100})
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("Inspect = %v, want ErrLimitExceeded", err)
	}
}

func TestVerifyDetectsCorruptContent(t *testing.T) {
	m := baseManifest(Entry{Path: "a", Type: EntryFile, Size: 1, Mode: 0o644, SHA256: sumOfX})
	data := craft(t, m, []craftEntry{{PayloadPrefix, fs.ModeDir | 0o755, ""}, {"persist/a", 0o644, "y"}})
	if _, err := Inspect(bytes.NewReader(data), int64(len(data)), Limits{}); err != nil {
		t.Fatalf("Inspect should pass structurally: %v", err)
	}
	if _, err := Verify(bytes.NewReader(data), int64(len(data)), Limits{}); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Verify = %v, want ErrInvalidArchive", err)
	}
	if _, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data)), t.TempDir(), ExtractOptions{}); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Extract = %v, want ErrInvalidArchive", err)
	}
}

func TestZipBombRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.CreateHeader(&zip.FileHeader{Name: PayloadPrefix})
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "persist/bomb", Method: zip.Deflate})
	w.Write(bytes.Repeat([]byte{0}, 64<<20))
	mw, _ := zw.Create(ManifestName)
	json.NewEncoder(mw).Encode(baseManifest())
	zw.Close()
	data := buf.Bytes()
	if _, err := Inspect(bytes.NewReader(data), int64(len(data)), Limits{}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("Inspect = %v, want ErrLimitExceeded", err)
	}
}

func TestExtractRefusesNonEmptyDestination(t *testing.T) {
	data, _ := export(t, buildTree(t))
	dst := t.TempDir()
	os.WriteFile(filepath.Join(dst, "existing"), []byte("keep"), 0o644)
	_, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data)), dst, ExtractOptions{})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Extract = %v, want not-empty refusal", err)
	}
}

func TestVerifyRestoreDetectsTampering(t *testing.T) {
	data, m := export(t, buildTree(t))
	dst := t.TempDir()
	if _, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data)), dst, ExtractOptions{}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dst, "agentroot/stray"), []byte("x"), 0o644)
	if err := VerifyRestore(context.Background(), dst, m, nil, nil); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("VerifyRestore = %v, want ErrInvalidArchive", err)
	}
}

func TestCleanRelPath(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{".", true}, {"a", true}, {"a/b/.c", true},
		{"", false}, {"/a", false}, {"a/../b", false}, {"a//b", false}, {"./a", false},
		{"a/", false}, {"a\x00", false}, {"..", false},
		{`unit\x2dname.service`, true}, {`a\..\b`, false}, {`\a`, false}, {"bad\xff", false},
	}
	for _, tc := range tests {
		_, err := CleanRelPath(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("CleanRelPath(%q) err = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestUnportableNamesAreListedAndTheArchiveStillVerifies(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{`dev-disk-by\x2duuid.mount`, "bad\xffname", `x\..\y`} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Skipf("filesystem refuses %q: %v", name, err)
		}
	}
	data, m := export(t, root)
	if _, err := Verify(bytes.NewReader(data), int64(len(data)), Limits{}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(m.Excluded) != 2 {
		t.Errorf("excluded = %+v, want the non-UTF-8 and the traversal-looking names", m.Excluded)
	}
	var kept bool
	for _, e := range m.Entries {
		kept = kept || e.Path == `dev-disk-by\x2duuid.mount`
	}
	if !kept {
		t.Error("an ordinary backslash name was not archived")
	}
}

func TestRestrictiveDirectoryModesRestore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "locked/inner"), 0o755)
	os.WriteFile(filepath.Join(root, "locked/inner/f"), []byte("x"), 0o644)
	os.Chmod(filepath.Join(root, "locked"), 0o500)
	defer os.Chmod(filepath.Join(root, "locked"), 0o755)
	data, m := export(t, root)
	dst := t.TempDir()
	if _, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data)), dst, ExtractOptions{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	defer os.Chmod(filepath.Join(dst, "locked"), 0o755)
	if err := VerifyRestore(context.Background(), dst, m, nil, nil); err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
}

// The walk's size estimate must never undercount the stream: any archive
// that would exceed the upload limit has to fail during the walk.
func TestSizeEstimateCoversTheStream(t *testing.T) {
	root := buildTree(t)
	noise := make([]byte, 3<<20)
	for i := range noise {
		noise[i] = byte(i*7919 + i/13)
	}
	os.WriteFile(filepath.Join(root, "agentroot/noise.bin"), noise, 0o644)
	data, _ := export(t, root)
	_, err := Write(context.Background(), root, &bytes.Buffer{}, WriteOptions{Scan: ScanOptions{Exclude: DefaultExclusions(), MaxBytes: int64(len(data)) - 1}})
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("a limit one byte under the real stream (%d) was not caught during the walk: %v", len(data), err)
	}
}

func TestExtractSkipsCredentialFiles(t *testing.T) {
	data, m := export(t, buildTree(t))
	skip := map[string]bool{}
	for _, p := range SensitivePaths(m) {
		skip[p] = true
	}
	if !skip["agentroot/home/kyber/.claude/.credentials.json"] {
		t.Fatalf("SensitivePaths = %v, want the Claude credentials file", SensitivePaths(m))
	}
	dst := t.TempDir()
	if _, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data)), dst, ExtractOptions{Skip: skip}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "agentroot/home/kyber/.claude/.credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("skipped credential file exists after restore: %v", err)
	}
	if err := VerifyRestore(context.Background(), dst, m, nil, skip); err != nil {
		t.Fatalf("VerifyRestore with skips: %v", err)
	}
	if err := VerifyRestore(context.Background(), dst, m, nil, nil); err == nil {
		t.Fatal("VerifyRestore without the skip set accepted a missing file")
	}
}

func TestCronPaths(t *testing.T) {
	m := &Manifest{Entries: []Entry{
		{Path: "agentroot/var/spool/cron/crontabs/kyber", Type: EntryFile, Size: 10},
		{Path: "agentroot/etc/cron.d/backup", Type: EntryFile, Size: 10},
		{Path: "agentroot/etc/cron.d/kyber-jobs", Type: EntryFile, Size: 10},
		{Path: "agentroot/etc/cron.d/empty", Type: EntryFile, Size: 0},
		{Path: "agentroot/home/kyber/notes", Type: EntryFile, Size: 10},
	}}
	got := CronPaths(m)
	if len(got) != 2 || got[0] != "agentroot/var/spool/cron/crontabs/kyber" || got[1] != "agentroot/etc/cron.d/backup" {
		t.Errorf("CronPaths = %v", got)
	}
}

func TestSkipIfRequiresSkipMap(t *testing.T) {
	data, _ := export(t, buildTree(t))
	_, err := Extract(context.Background(), bytes.NewReader(data), int64(len(data)), t.TempDir(), ExtractOptions{SkipIf: IsSensitive})
	if err == nil {
		t.Fatal("Extract accepted SkipIf without a Skip map")
	}
}
