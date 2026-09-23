package diskarchive

import (
	"archive/zip"
	"bufio"
	"compress/flate"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

// WriteOptions configures an export.
type WriteOptions struct {
	Scan   ScanOptions
	Source Source
	Mounts []Mount
	// Now is the export timestamp; zero means time.Now.
	Now time.Time
}

// DefaultExclusions are paths under a volume root that are filesystem
// metadata rather than agent state.
func DefaultExclusions() map[string]string {
	return map[string]string{
		"lost+found": "filesystem metadata recreated by mkfs, not agent state",
	}
}

// Write streams a ZIP of rootDir to w and returns the manifest it embedded.
// Output is produced incrementally; nothing is staged on disk. The manifest is
// the final entry, written once every checksum is known. Any error leaves w
// holding an incomplete archive the caller must discard.
func Write(ctx context.Context, rootDir string, w io.Writer, opts WriteOptions) (*Manifest, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("opening root: %w", err)
	}
	defer root.Close()

	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	m := &Manifest{
		FormatVersion: FormatVersion,
		ExportedAt:    now.UTC(),
		Source:        opts.Source,
		Mounts:        opts.Mounts,
	}
	s := &scanner{root: root, opts: opts.Scan}
	err = s.walk(ctx, ".", func(item scanItem) error {
		e := item.entry
		hdr := &zip.FileHeader{Name: e.ZipName(), Modified: e.ModTime}
		switch e.Type {
		case EntryDir:
			hdr.SetMode(fs.ModeDir | fs.FileMode(e.Mode&0o777))
			if _, err := zw.CreateHeader(hdr); err != nil {
				return fmt.Errorf("writing %s: %w", e.Path, err)
			}
			m.Totals.Dirs++
		case EntrySymlink:
			hdr.Method = zip.Store
			hdr.SetMode(fs.ModeSymlink | 0o777)
			fw, err := zw.CreateHeader(hdr)
			if err != nil {
				return fmt.Errorf("writing %s: %w", e.Path, err)
			}
			if _, err := io.WriteString(fw, e.Target); err != nil {
				return fmt.Errorf("writing %s: %w", e.Path, err)
			}
			m.Totals.Symlinks++
		case EntryFile:
			hdr.Method = zip.Deflate
			hdr.SetMode(fs.FileMode(e.Mode & 0o777))
			fw, err := zw.CreateHeader(hdr)
			if err != nil {
				return fmt.Errorf("writing %s: %w", e.Path, err)
			}
			sum, err := hashFile(root, item, fw)
			if err != nil {
				return err
			}
			e.SHA256 = sum
			m.Totals.Files++
			m.Totals.Bytes += e.Size
		}
		m.Entries = append(m.Entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	m.Excluded = s.excluded
	m.Totals.Entries = len(m.Entries)

	fw, err := zw.CreateHeader(&zip.FileHeader{Name: ManifestName, Method: zip.Deflate, Modified: m.ExportedAt})
	if err != nil {
		return nil, fmt.Errorf("writing manifest: %w", err)
	}
	if err := writeManifest(fw, m); err != nil {
		return nil, fmt.Errorf("writing manifest: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("finishing archive: %w", err)
	}
	return m, nil
}

// writeManifest streams the manifest one entry at a time. Encoding it whole
// would build a second copy of a manifest that, on a disk with millions of
// files, is hundreds of megabytes.
func writeManifest(w io.Writer, m *Manifest) error {
	head := *m
	head.Entries = nil
	b, err := json.Marshal(head)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 256<<10)
	// head ends in "}"; reopen it for the entries (Entries is the last field).
	if _, err := bw.Write(b[:len(b)-1]); err != nil {
		return err
	}
	if _, err := bw.WriteString(`,"entries":[`); err != nil {
		return err
	}
	for i, e := range m.Entries {
		if i > 0 {
			if err := bw.WriteByte(','); err != nil {
				return err
			}
		}
		eb, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := bw.Write(eb); err != nil {
			return err
		}
	}
	if _, err := bw.WriteString("]}\n"); err != nil {
		return err
	}
	return bw.Flush()
}
