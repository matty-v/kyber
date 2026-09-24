package diskarchive

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// RootfsDir is the agent's durable root on its volume under rootfs
// persistence; the image manifest's paths are relative to it.
const RootfsDir = "agentroot"

// ImageManifestPath is where kyber-rootfs records the base image's files, on
// the volume an archive is taken from.
const ImageManifestPath = "kyber/rootfs-image-manifest.tsv"

// ImageFile is one regular file of the base image as kyber-rootfs recorded it.
type ImageFile struct {
	Size    int64
	ModTime time.Time
}

// Matches reports whether e is still the image's copy of the file: a regular
// file of the same size whose modification time is not newer. The zero
// ImageFile matches nothing.
func (f ImageFile) Matches(e Entry) bool {
	return !f.ModTime.IsZero() && e.Type == EntryFile && e.Size == f.Size && !e.ModTime.After(f.ModTime)
}

// ParseImageManifest reads kyber-rootfs's image manifest (tab-separated type,
// size, mtime, mode and "./"-relative path; lines starting with "#" are
// headers) and returns its regular files keyed by archive path under prefix.
// Lines it cannot read are skipped: the manifest only ever narrows what is
// flagged, so a missing line errs toward flagging.
func ParseImageManifest(r io.Reader, prefix string) (map[string]ImageFile, error) {
	out := map[string]ImageFile{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 || f[0] != "f" || !strings.HasPrefix(f[4], "./") {
			continue
		}
		size, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		mtime, ok := parseEpoch(f[2])
		if !ok {
			continue
		}
		out[prefix+strings.TrimPrefix(f[4], "./")] = ImageFile{Size: size, ModTime: mtime}
	}
	return out, sc.Err()
}

// parseEpoch parses find's %T@ ("seconds.fraction") exactly, without a
// float's rounding.
func parseEpoch(s string) (time.Time, bool) {
	secs, frac, _ := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	frac += strings.Repeat("0", 9-len(frac))
	nsec, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, nsec), true
}
