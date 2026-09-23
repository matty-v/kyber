package archivestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// FilesystemStore keeps objects as files under a directory. It backs the
// built-in archive store server and tests.
type FilesystemStore struct {
	root *os.Root
	dir  string
}

// NewFilesystemStore opens dir, creating it if needed.
func NewFilesystemStore(dir string) (*FilesystemStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating store directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening store directory: %w", err)
	}
	return &FilesystemStore{root: root, dir: dir}, nil
}

func (s *FilesystemStore) Name() string { return "filesystem" }

// Put writes to a temporary sibling and renames it into place only after the
// body is complete and synced, so a failed or interrupted upload never leaves
// a readable partial object.
func (s *FilesystemStore) Put(ctx context.Context, key string, body io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	if dir := filepath.Dir(key); dir != "." {
		if err := s.root.MkdirAll(dir, 0o700); err != nil {
			return 0, fmt.Errorf("creating object directory: %w", err)
		}
	}
	tmp := key + ".partial"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("creating object: %w", err)
	}
	n, err := io.Copy(f, ctxReader{ctx, body})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.root.Rename(tmp, key)
	}
	if err != nil {
		_ = s.root.Remove(tmp)
		return 0, fmt.Errorf("writing object: %w", err)
	}
	return n, nil
}

func (s *FilesystemStore) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	f, err := s.root.Open(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("opening object: %w", err)
	}
	if offset < 0 {
		f.Close()
		return nil, errors.New("archivestore: negative offset")
	}
	if length < 0 {
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		length = info.Size() - offset
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, offset, length), f}, nil
}

func (s *FilesystemStore) Size(ctx context.Context, key string) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	info, err := s.root.Stat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *FilesystemStore) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	for _, k := range []string{key, key + ".partial"} {
		if err := s.root.Remove(k); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("deleting object: %w", err)
		}
	}
	return nil
}

// Capacity reports the backing filesystem's space.
func (s *FilesystemStore) Capacity(ctx context.Context) (Capacity, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(s.dir, &st); err != nil {
		return Capacity{}, fmt.Errorf("statfs: %w", err)
	}
	return Capacity{
		TotalBytes:     int64(st.Blocks) * int64(st.Bsize),
		AvailableBytes: int64(st.Bavail) * int64(st.Bsize),
	}, nil
}

// SweepPartials removes uploads interrupted by a restart.
func (s *FilesystemStore) SweepPartials() error {
	return filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".partial") {
			return os.Remove(p)
		}
		return nil
	})
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
