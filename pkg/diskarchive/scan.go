package diskarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
)

// ScanOptions controls a walk of an archived or restored root.
type ScanOptions struct {
	// Exclude maps root-relative paths to the reason they are left out. An
	// excluded directory is not descended into.
	Exclude map[string]string
	// MaxEntries and MaxBytes bound the walk; zero means unbounded.
	// MaxBytes bounds the estimated size of the archive stream (file data
	// plus ZIP headers and the manifest), the same quantity the control
	// plane enforces on upload, so an export that cannot fit fails while
	// walking rather than after the whole disk has been read.
	MaxEntries int
	MaxBytes   int64
}

// scanItem is one object found by scan. For regular files, open returns the
// file positioned at offset zero; the walker verifies after reading that the
// file did not change underneath it.
type scanItem struct {
	entry Entry
	info  fs.FileInfo
}

// scanner walks root through os.Root so no path it opens can escape the root,
// even through a symlink planted mid-walk.
type scanner struct {
	root     *os.Root
	opts     ScanOptions
	entries  int
	bytes    int64
	names    int64
	excluded []Exclusion
}

// walk visits every object under the root in lexical order, parents before
// children. Special files (sockets, FIFOs, devices) cannot be represented
// portably; they are recorded as exclusions with a reason, never skipped
// silently.
func (s *scanner) walk(ctx context.Context, rel string, visit func(scanItem) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reason, ok := s.opts.Exclude[rel]; ok && rel != "." {
		s.excluded = append(s.excluded, Exclusion{Path: rel, Reason: reason})
		return nil
	}
	// A name the archive cannot carry safely (not UTF-8, or one that a ZIP
	// tool could read as traversal) is listed rather than failing the whole
	// export after the agent was stopped. Such names are vanishingly rare on
	// agent disks; the manifest says exactly which were left out.
	if _, err := CleanRelPath(rel); err != nil {
		s.excluded = append(s.excluded, Exclusion{Path: strings.ToValidUTF8(rel, "\uFFFD"),
			Reason: "file name cannot be stored safely in a portable archive (not UTF-8, or ambiguous separators)"})
		return nil
	}
	info, err := s.root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("%w: lstat %s: %v", ErrChanged, rel, err)
	}
	entry, special, err := entryFromInfo(rel, info)
	if err != nil {
		return err
	}
	if special != "" {
		s.excluded = append(s.excluded, Exclusion{Path: rel, Reason: special})
		return nil
	}
	if entry.Type == EntrySymlink {
		target, err := s.root.Readlink(rel)
		if err != nil {
			return fmt.Errorf("%w: readlink %s: %v", ErrChanged, rel, err)
		}
		entry.Target = target
		entry.Size = 0
	}
	s.entries++
	if s.opts.MaxEntries > 0 && s.entries > s.opts.MaxEntries {
		return fmt.Errorf("%w: more than %d entries", ErrLimitExceeded, s.opts.MaxEntries)
	}
	if entry.Type == EntryFile {
		s.bytes += entry.Size
	}
	s.names += int64(len(rel))
	if s.opts.MaxBytes > 0 && s.estimate() > s.opts.MaxBytes {
		return fmt.Errorf("%w: archive would exceed %d bytes", ErrLimitExceeded, s.opts.MaxBytes)
	}
	if err := visit(scanItem{entry: entry, info: info}); err != nil {
		return err
	}
	if entry.Type != EntryDir {
		return nil
	}
	dir, err := s.root.Open(rel)
	if err != nil {
		return fmt.Errorf("%w: open dir %s: %v", ErrChanged, rel, err)
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return fmt.Errorf("%w: read dir %s: %v", ErrChanged, rel, err)
	}
	slices.Sort(names)
	for _, name := range names {
		child := name
		if rel != "." {
			child = rel + "/" + name
		}
		if err := s.walk(ctx, child, visit); err != nil {
			return err
		}
	}
	return nil
}

// estimate bounds the archive stream the walk so far will produce: file data
// with deflate's worst-case expansion, per-entry local and central ZIP
// headers and data descriptors, and the manifest record for each entry.
func (s *scanner) estimate() int64 {
	const perEntry = 400 // headers, descriptor and a manifest record
	return s.bytes + s.bytes/1000 + int64(s.entries)*perEntry + 3*s.names
}

// entryFromInfo converts lstat output into an Entry. A non-empty special
// return names why the object cannot be archived.
func entryFromInfo(rel string, info fs.FileInfo) (Entry, string, error) {
	e := Entry{
		Path:    rel,
		Mode:    uint32(info.Mode().Perm()),
		ModTime: info.ModTime().UTC(),
		Size:    info.Size(),
	}
	m := info.Mode()
	if m&fs.ModeSetuid != 0 {
		e.Mode |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		e.Mode |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		e.Mode |= 0o1000
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.UID = int(st.Uid)
		e.GID = int(st.Gid)
	}
	switch {
	case m.IsDir():
		e.Type = EntryDir
		e.Size = 0
	case m.IsRegular():
		e.Type = EntryFile
	case m&fs.ModeSymlink != 0:
		e.Type = EntrySymlink
	case m&fs.ModeSocket != 0:
		return e, "socket: not portable, recreated by the process that owns it", nil
	case m&fs.ModeNamedPipe != 0:
		return e, "named pipe: not portable, recreated by the process that owns it", nil
	case m&fs.ModeDevice != 0:
		return e, "device node: not portable", nil
	default:
		return e, "", fmt.Errorf("%w: %s has unsupported file type %v", ErrInvalidArchive, rel, m.Type())
	}
	return e, "", nil
}

// hashFile streams a regular file into w (when non-nil) while computing its
// SHA-256, then confirms the file is the same object with the same size and
// modification time it had when it was listed. A file that changed or could
// not be read fails the export; it is never silently omitted.
func hashFile(root *os.Root, item scanItem, w io.Writer) (string, error) {
	f, err := root.OpenFile(item.entry.Path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("%w: open %s: %v", ErrChanged, item.entry.Path, err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !os.SameFile(before, item.info) {
		return "", fmt.Errorf("%w: %s was replaced", ErrChanged, item.entry.Path)
	}
	h := sha256.New()
	dst := io.Writer(h)
	if w != nil {
		dst = io.MultiWriter(h, w)
	}
	n, err := io.Copy(dst, f)
	if err != nil {
		return "", fmt.Errorf("%w: read %s: %v", ErrChanged, item.entry.Path, err)
	}
	after, err := f.Stat()
	if err != nil || n != item.entry.Size || after.Size() != item.entry.Size || !after.ModTime().Equal(item.info.ModTime()) {
		return "", fmt.Errorf("%w: %s changed while it was read", ErrChanged, item.entry.Path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Scan walks root and returns every entry with regular-file checksums, in the
// same order and shape Write records them. A restore uses it to verify the
// destination against the manifest.
func Scan(ctx context.Context, rootDir string, opts ScanOptions) ([]Entry, []Exclusion, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, nil, fmt.Errorf("opening root: %w", err)
	}
	defer root.Close()
	s := &scanner{root: root, opts: opts}
	var entries []Entry
	err = s.walk(ctx, ".", func(item scanItem) error {
		if item.entry.Type == EntryFile {
			sum, err := hashFile(root, item, nil)
			if err != nil {
				return err
			}
			item.entry.SHA256 = sum
		}
		entries = append(entries, item.entry)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return entries, s.excluded, nil
}

// CompareEntries reports the first difference between the manifest's entries
// and a scan of a restored tree. Symlink modification times are not compared:
// they cannot be set portably without following the link.
func CompareEntries(want, got []Entry) error {
	byPath := make(map[string]Entry, len(got))
	for _, e := range got {
		byPath[e.Path] = e
	}
	for _, w := range want {
		g, ok := byPath[w.Path]
		if !ok {
			return fmt.Errorf("%w: %s missing after restore", ErrInvalidArchive, w.Path)
		}
		delete(byPath, w.Path)
		timesMatch := w.Type == EntrySymlink || w.Path == "." || g.ModTime.Equal(w.ModTime)
		if g.Type != w.Type || g.Size != w.Size || g.Mode != w.Mode || g.UID != w.UID || g.GID != w.GID ||
			g.Target != w.Target || g.SHA256 != w.SHA256 || !timesMatch {
			return fmt.Errorf("%w: %s differs after restore", ErrInvalidArchive, w.Path)
		}
	}
	for p := range byPath {
		return fmt.Errorf("%w: unexpected %s after restore", ErrInvalidArchive, p)
	}
	return nil
}

// parentOf returns the archive-relative parent of p ("." for top level).
func parentOf(p string) string {
	d := path.Dir(p)
	if d == "" {
		return "."
	}
	return d
}
