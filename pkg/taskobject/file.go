package taskobject

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

const TaskResultsRoot = "/persist/task-results"

var (
	ErrUnsafePath   = errors.New("unsafe task result path")
	ErrUnsafeFile   = errors.New("unsafe task result file")
	ErrFileTooLarge = errors.New("task result file too large")
	ErrFileChanged  = errors.New("task result file changed while reading")
)

// SafeFile is an already-open regular file anchored beneath a trusted root.
// Reaching the declared size verifies that its identity and metadata did not
// change during streaming, without requiring the caller to perform an extra
// read after the last byte.
type SafeFile struct {
	f        *os.File
	initial  unix.Stat_t
	remain   int64
	finished bool
}

func (f *SafeFile) Size() int64 { return f.initial.Size }

func (f *SafeFile) Read(p []byte) (int, error) {
	if f.finished {
		return 0, io.EOF
	}
	if f.remain == 0 {
		var one [1]byte
		n, err := f.f.Read(one[:])
		if n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return n, ErrFileChanged
		}
		if err := f.verify(); err != nil {
			return 0, err
		}
		f.finished = true
		return 0, io.EOF
	}
	if int64(len(p)) > f.remain {
		p = p[:f.remain]
	}
	n, err := f.f.Read(p)
	f.remain -= int64(n)
	if errors.Is(err, io.EOF) && f.remain != 0 {
		return n, ErrFileChanged
	}
	if f.remain == 0 {
		var one [1]byte
		extra, extraErr := f.f.Read(one[:])
		if extra != 0 || (extraErr != nil && !errors.Is(extraErr, io.EOF)) {
			return n, ErrFileChanged
		}
		if verifyErr := f.verify(); verifyErr != nil {
			return n, verifyErr
		}
		f.finished = true
	}
	return n, err
}

func (f *SafeFile) Close() error { return f.f.Close() }

func (f *SafeFile) verify() error {
	var now unix.Stat_t
	if err := unix.Fstat(int(f.f.Fd()), &now); err != nil {
		return fmt.Errorf("restat task result: %w", err)
	}
	if now.Dev != f.initial.Dev || now.Ino != f.initial.Ino || now.Size != f.initial.Size ||
		now.Mtim != f.initial.Mtim || now.Ctim != f.initial.Ctim || now.Nlink != 1 {
		return ErrFileChanged
	}
	return nil
}

// OpenTaskResult opens rel beneath root without following symlinks in any path
// component. root should be TaskResultsRoot in production; it is configurable
// to permit hermetic tests.
func OpenTaskResult(root, rel string, maxSize int64) (*SafeFile, error) {
	if maxSize < 0 {
		return nil, errors.New("maximum task result size must not be negative")
	}
	parts, err := safeParts(rel)
	if err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open task results root: %w", err)
	}
	current := rootFD
	defer func() { _ = unix.Close(current) }()
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsafePath, openErr)
		}
		_ = unix.Close(current)
		current = next
	}
	fd, err := unix.Openat(current, parts[len(parts)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open task result: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("stat task result: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, ErrUnsafeFile
	}
	if st.Size > maxSize {
		_ = unix.Close(fd)
		return nil, ErrFileTooLarge
	}
	// The name is diagnostic only. Keep user-controlled path data out of the
	// os.File value after the descriptor has been opened and validated.
	file := os.NewFile(uintptr(fd), "task-result")
	return &SafeFile{f: file, initial: st, remain: st.Size}, nil
}

func safeParts(rel string) ([]string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.ContainsRune(rel, '\x00') {
		return nil, ErrUnsafePath
	}
	if filepath.Clean(rel) != rel {
		return nil, ErrUnsafePath
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, ErrUnsafePath
		}
	}
	return parts, nil
}

// SniffMediaType returns a content-derived media type without consuming r.
func SniffMediaType(r *bufio.Reader) (string, error) {
	b, err := r.Peek(512)
	if err != nil && !errors.Is(err, bufio.ErrBufferFull) && !errors.Is(err, io.EOF) {
		return "", err
	}
	return http.DetectContentType(b), nil
}

// SanitizeFilename creates a safe Content-Disposition filename. It deliberately
// discards path components and control/non-ASCII characters.
func SanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r > unicode.MaxASCII || unicode.IsControl(r) || strings.ContainsRune(`<>:"/\\|?*`, r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	name = strings.Trim(b.String(), " .")
	if name == "" || name == "." || name == ".." {
		name = "download"
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}
