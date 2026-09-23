package diskarchive

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// Limits bound what an untrusted archive may declare. Zero fields are
// unbounded except MaxCompressionRatio, which defaults to 1000.
type Limits struct {
	MaxEntries int
	// MaxBytes bounds the sum of declared regular-file sizes; a restore sets it
	// to the destination volume's usable capacity.
	MaxBytes int64
	// MaxCompressionRatio rejects entries whose declared size exceeds their
	// stored size by more than this factor (zip bombs). Entries under 1 MiB
	// are exempt: tiny highly compressible files are normal.
	MaxCompressionRatio uint64
}

// Inspected is a structurally validated archive: the manifest and the ZIP
// entries have been matched one to one, but file contents have not been read.
type Inspected struct {
	Manifest *Manifest
	zr       *zip.Reader
	files    map[string]*zip.File
}

// Inspect validates an archive's structure without reading file contents:
// format version, canonical and unique paths, parent chains, types, modes,
// declared sizes, limits, and a one-to-one match between manifest entries and
// ZIP entries. Anything the manifest does not describe is rejected.
func Inspect(ra io.ReaderAt, size int64, lim Limits) (*Inspected, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArchive, err)
	}
	ratio := lim.MaxCompressionRatio
	if ratio == 0 {
		ratio = 1000
	}
	files := make(map[string]*zip.File, len(zr.File))
	var manifestFile *zip.File
	for _, f := range zr.File {
		if _, dup := files[f.Name]; dup || f.Name == ManifestName && manifestFile != nil {
			return nil, fmt.Errorf("%w: duplicate entry %q", ErrInvalidArchive, f.Name)
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return nil, fmt.Errorf("%w: %q uses unsupported compression method %d", ErrInvalidArchive, f.Name, f.Method)
		}
		if f.UncompressedSize64 > 1<<20 && (f.CompressedSize64 == 0 || f.UncompressedSize64/f.CompressedSize64 > ratio) {
			return nil, fmt.Errorf("%w: %q compression ratio exceeds %d", ErrLimitExceeded, f.Name, ratio)
		}
		if f.Name == ManifestName {
			manifestFile = f
			continue
		}
		files[f.Name] = f
	}
	if manifestFile == nil {
		return nil, fmt.Errorf("%w: no %s", ErrInvalidArchive, ManifestName)
	}
	if manifestFile.UncompressedSize64 > maxManifestBytes {
		return nil, fmt.Errorf("%w: manifest larger than %d bytes", ErrLimitExceeded, maxManifestBytes)
	}
	rc, err := manifestFile.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: opening manifest: %v", ErrInvalidArchive, err)
	}
	var m Manifest
	err = json.NewDecoder(io.LimitReader(rc, maxManifestBytes)).Decode(&m)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: decoding manifest: %v", ErrInvalidArchive, err)
	}
	if err := CheckVersion(m.FormatVersion); err != nil {
		return nil, err
	}
	if lim.MaxEntries > 0 && len(m.Entries) > lim.MaxEntries {
		return nil, fmt.Errorf("%w: %d entries exceeds %d", ErrLimitExceeded, len(m.Entries), lim.MaxEntries)
	}
	if len(m.Entries) != len(files) {
		return nil, fmt.Errorf("%w: manifest lists %d entries, archive holds %d", ErrInvalidArchive, len(m.Entries), len(files))
	}

	types := make(map[string]EntryType, len(m.Entries))
	var totals Totals
	for i, e := range m.Entries {
		p, err := CleanRelPath(e.Path)
		if err != nil {
			return nil, err
		}
		if (i == 0) != (p == ".") {
			return nil, fmt.Errorf("%w: the root entry must come first, exactly once", ErrInvalidArchive)
		}
		if _, dup := types[p]; dup {
			return nil, fmt.Errorf("%w: duplicate manifest path %q", ErrInvalidArchive, p)
		}
		if p != "." && types[parentOf(p)] != EntryDir {
			return nil, fmt.Errorf("%w: %q has no directory parent earlier in the manifest", ErrInvalidArchive, p)
		}
		if e.Mode&^0o7777 != 0 || e.Size < 0 || e.UID < 0 || e.GID < 0 {
			return nil, fmt.Errorf("%w: %q has invalid metadata", ErrInvalidArchive, p)
		}
		f, ok := files[e.ZipName()]
		if !ok {
			return nil, fmt.Errorf("%w: manifest entry %q has no archive entry", ErrInvalidArchive, p)
		}
		mode := f.Mode()
		switch e.Type {
		case EntryDir:
			if !mode.IsDir() || e.Size != 0 {
				return nil, fmt.Errorf("%w: %q type mismatch", ErrInvalidArchive, p)
			}
			totals.Dirs++
		case EntryFile:
			if !mode.IsRegular() || f.UncompressedSize64 != uint64(e.Size) || !isSHA256(e.SHA256) {
				return nil, fmt.Errorf("%w: %q does not match its manifest record", ErrInvalidArchive, p)
			}
			totals.Files++
			totals.Bytes += e.Size
		case EntrySymlink:
			if mode&fs.ModeSymlink == 0 || e.Target == "" || strings.ContainsRune(e.Target, 0) ||
				f.UncompressedSize64 != uint64(len(e.Target)) {
				return nil, fmt.Errorf("%w: %q does not match its manifest record", ErrInvalidArchive, p)
			}
			totals.Symlinks++
		default:
			return nil, fmt.Errorf("%w: %q has unsupported type %q", ErrInvalidArchive, p, e.Type)
		}
		if lim.MaxBytes > 0 && totals.Bytes > lim.MaxBytes {
			return nil, fmt.Errorf("%w: archived files exceed %d bytes", ErrLimitExceeded, lim.MaxBytes)
		}
		types[p] = e.Type
	}
	totals.Entries = len(m.Entries)
	if totals != m.Totals {
		return nil, fmt.Errorf("%w: manifest totals do not match its entries", ErrInvalidArchive)
	}
	return &Inspected{Manifest: &m, zr: zr, files: files}, nil
}

// Verify is Inspect plus a full content check: every regular file's SHA-256
// and every symlink target are read back from the archive and compared with
// the manifest.
func Verify(ra io.ReaderAt, size int64, lim Limits) (*Manifest, error) {
	in, err := Inspect(ra, size, lim)
	if err != nil {
		return nil, err
	}
	for _, e := range in.Manifest.Entries {
		switch e.Type {
		case EntryFile:
			if err := in.copyFile(e, io.Discard); err != nil {
				return nil, err
			}
		case EntrySymlink:
			if _, err := in.readTarget(e); err != nil {
				return nil, err
			}
		}
	}
	return in.Manifest, nil
}

// copyFile streams e's content into w and fails unless it hashes to the
// manifest's checksum. archive/zip also enforces the declared size and CRC.
func (in *Inspected) copyFile(e Entry, w io.Writer) error {
	rc, err := in.files[e.ZipName()].Open()
	if err != nil {
		return fmt.Errorf("%w: opening %q: %v", ErrInvalidArchive, e.Path, err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(h, w), io.LimitReader(rc, e.Size+1))
	if err != nil {
		return fmt.Errorf("%w: reading %q: %v", ErrInvalidArchive, e.Path, err)
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("%w: %q content does not match its checksum", ErrInvalidArchive, e.Path)
	}
	return nil
}

func (in *Inspected) readTarget(e Entry) (string, error) {
	rc, err := in.files[e.ZipName()].Open()
	if err != nil {
		return "", fmt.Errorf("%w: opening %q: %v", ErrInvalidArchive, e.Path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, int64(len(e.Target))+1))
	if err != nil || string(b) != e.Target {
		return "", fmt.Errorf("%w: %q symlink target does not match the manifest", ErrInvalidArchive, e.Path)
	}
	return e.Target, nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
