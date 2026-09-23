package diskarchive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"syscall"
)

// ExtractOptions configures a restore.
type ExtractOptions struct {
	Limits Limits
	// PreserveOwnership applies each entry's uid/gid. It requires root and is
	// what a restore pod does; tests running unprivileged turn it off.
	PreserveOwnership bool
	// Allowed lists top-level names that may already exist in the destination
	// (for example "lost+found" on a fresh ext4 volume).
	Allowed []string
	// Skip names regular files the restore deliberately leaves out (for
	// example the source harness's credential files). Only regular files can
	// be skipped, so a skip can never orphan a directory's children.
	Skip map[string]bool
	// SkipIf leaves out any regular file it matches, evaluated against the
	// full manifest (not a truncated summary). Extract adds matches to Skip
	// so VerifyRestore can be given the same set.
	SkipIf func(Entry) bool
}

// Extract restores a structurally valid archive into destDir, which must be
// empty apart from opts.Allowed names. All writes go through os.Root, so no
// entry can land outside destDir. Directories are created first, then files
// (hashed as they are written), then symlinks, so no write ever traverses a
// symlink taken from the archive. Metadata is applied last: ownership, then
// modes, then timestamps deepest-first so creating children does not disturb a
// parent's restored mtime.
//
// On error the destination holds a partial restore; the caller discards the
// volume. Extract never starts anything from it.
func Extract(ctx context.Context, ra io.ReaderAt, size int64, destDir string, opts ExtractOptions) (*Manifest, error) {
	in, err := Inspect(ra, size, opts.Limits)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return nil, fmt.Errorf("opening destination: %w", err)
	}
	defer root.Close()
	if err := requireEmpty(root, opts.Allowed); err != nil {
		return nil, err
	}

	m := in.Manifest
	if opts.SkipIf != nil {
		// The caller passes the same map to VerifyRestore; a private map here
		// would make every verification report the skipped files missing.
		if opts.Skip == nil {
			return nil, errors.New("diskarchive: SkipIf needs a Skip map to record what it skipped")
		}
		for _, e := range m.Entries {
			if e.Type == EntryFile && opts.SkipIf(e) {
				opts.Skip[e.Path] = true
			}
		}
	}
	for _, e := range m.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch e.Type {
		case EntryDir:
			if e.Path == "." {
				continue
			}
			if err := root.Mkdir(e.Path, 0o700); err != nil {
				return nil, fmt.Errorf("creating %s: %w", e.Path, err)
			}
		case EntryFile:
			if opts.Skip[e.Path] {
				continue
			}
			f, err := root.OpenFile(e.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
			if err != nil {
				return nil, fmt.Errorf("creating %s: %w", e.Path, err)
			}
			err = in.copyFile(e, f)
			if cerr := f.Close(); err == nil && cerr != nil {
				err = fmt.Errorf("writing %s: %w", e.Path, cerr)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	for _, e := range m.Entries {
		if e.Type != EntrySymlink {
			continue
		}
		target, err := in.readTarget(e)
		if err != nil {
			return nil, err
		}
		if err := root.Symlink(target, e.Path); err != nil {
			return nil, fmt.Errorf("creating symlink %s: %w", e.Path, err)
		}
	}

	if opts.PreserveOwnership {
		for _, e := range m.Entries {
			if e.Type == EntryFile && opts.Skip[e.Path] {
				continue
			}
			if err := root.Lchown(e.Path, e.UID, e.GID); err != nil {
				return nil, fmt.Errorf("chown %s: %w", e.Path, err)
			}
		}
	}
	// Modes and times deepest-first: a directory's restored mode may drop
	// search permission, and touching children must not disturb a parent's
	// restored mtime. chown clears setuid/setgid, so modes follow it.
	for _, e := range slices.Backward(m.Entries) {
		if e.Type == EntrySymlink || e.Type == EntryFile && opts.Skip[e.Path] {
			continue
		}
		if err := root.Chmod(e.Path, toFileMode(e.Mode)); err != nil {
			return nil, fmt.Errorf("chmod %s: %w", e.Path, err)
		}
		if err := root.Chtimes(e.Path, e.ModTime, e.ModTime); err != nil {
			return nil, fmt.Errorf("setting times on %s: %w", e.Path, err)
		}
	}
	return m, nil
}

// VerifyRestore re-scans a restored tree and compares every entry, including
// content checksums, with the manifest, less the files Extract skipped.
func VerifyRestore(ctx context.Context, destDir string, m *Manifest, allowed []string, skip map[string]bool) error {
	exclude := make(map[string]string, len(allowed))
	for _, name := range allowed {
		exclude[name] = "pre-existing on the destination volume"
	}
	got, _, err := Scan(ctx, destDir, ScanOptions{Exclude: exclude})
	if err != nil {
		return err
	}
	// The destination root's mtime moves when an allowed entry (such as
	// lost+found) is present; CompareEntries skips the root's time for that
	// reason.
	want := m.Entries
	if len(skip) > 0 {
		want = make([]Entry, 0, len(m.Entries))
		for _, e := range m.Entries {
			if !(e.Type == EntryFile && skip[e.Path]) {
				want = append(want, e)
			}
		}
	}
	return CompareEntries(want, got)
}

func requireEmpty(root *os.Root, allowed []string) error {
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("opening destination: %w", err)
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return fmt.Errorf("reading destination: %w", err)
	}
	for _, n := range names {
		if !slices.Contains(allowed, n) {
			return fmt.Errorf("destination is not empty: %q exists", n)
		}
	}
	return nil
}

func toFileMode(mode uint32) fs.FileMode {
	m := fs.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}
