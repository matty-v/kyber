package taskobject

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenTaskResult(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "nested", "result.txt")
	if err := os.WriteFile(p, []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenTaskResult(root, "nested/result.txt", 6)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "result" {
		t.Fatalf("read=%q err=%v", b, err)
	}
}

func TestOpenTaskResultRejectsUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oversized"), []byte("xx"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		want error
	}{
		{"../target", ErrUnsafePath}, {"link", nil}, {"hardlink", ErrUnsafeFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := OpenTaskResult(root, tc.name, 10)
			if f != nil {
				f.Close()
			}
			if tc.want == nil {
				if err == nil {
					t.Fatal("expected error")
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := OpenTaskResult(root, "oversized", 1); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestOpenTaskResultDetectsChange(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "result")
	if err := os.WriteFile(p, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenTaskResult(root, "result", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(f, buf[:1]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("abd"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := time.Now().Add(time.Second)
	if err := os.Chtimes(p, changed, changed); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Read(buf); n != 2 || !errors.Is(err, ErrFileChanged) {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestFilenameAndSniff(t *testing.T) {
	if got := SanitizeFilename(`../bad?.txt`); got != "bad_.txt" {
		t.Fatalf("got %q", got)
	}
	r := bufio.NewReader(strings.NewReader("hello"))
	ct, err := SniffMediaType(r)
	if err != nil {
		t.Fatal(err)
	}
	if ct != "text/plain; charset=utf-8" {
		t.Fatalf("got %q", ct)
	}
	b, _ := io.ReadAll(r)
	if string(b) != "hello" {
		t.Fatalf("reader consumed: %q", b)
	}
}
