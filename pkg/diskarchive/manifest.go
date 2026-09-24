// Package diskarchive defines Kyber's portable agent-disk archive: a ZIP of an
// agent's persistent volume plus a versioned manifest describing every entry
// (MAT-87), and the validation and extraction rules a restore applies before
// any byte reaches a destination volume (MAT-88).
//
// The archive is deliberately self-describing. The manifest is the contract;
// the ZIP entries are the payload. A reader never trusts one without the
// other: Verify rejects any archive whose entries and manifest disagree, and
// Extract refuses to write an entry the manifest does not describe.
//
// Layout:
//
//	persist/<relative path>        one entry per archived filesystem object
//	kyber-export/manifest.json     written last, after every checksum is known
package diskarchive

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// FormatVersion identifies the archive layout and manifest schema. A reader
// accepts only versions it lists in SupportedFormatVersions; an unknown
// version is rejected before anything is created (MAT-88).
const FormatVersion = "kyber.io/agent-disk-archive/v1"

// SupportedFormatVersions lists every FormatVersion this build can restore.
var SupportedFormatVersions = []string{FormatVersion}

const (
	// PayloadPrefix is the ZIP directory holding the archived volume.
	PayloadPrefix = "persist/"
	// ManifestName is the ZIP entry holding the manifest.
	ManifestName = "kyber-export/manifest.json"
	// maxManifestBytes bounds how much of an untrusted archive is decoded as
	// JSON. A million entries at a generous 512 bytes each fits comfortably.
	maxManifestBytes = 512 << 20
)

// EntryType is the kind of filesystem object an entry records.
type EntryType string

const (
	EntryDir     EntryType = "dir"
	EntryFile    EntryType = "file"
	EntrySymlink EntryType = "symlink"
)

// Sentinel errors callers branch on.
var (
	ErrUnsupportedVersion = errors.New("diskarchive: unsupported archive format version")
	ErrInvalidArchive     = errors.New("diskarchive: archive is invalid")
	ErrUnsafePath         = errors.New("diskarchive: unsafe path")
	ErrLimitExceeded      = errors.New("diskarchive: archive exceeds a configured limit")
	ErrChanged            = errors.New("diskarchive: file changed or became unreadable during export")
)

// Manifest describes one archive. Everything in it is non-secret metadata:
// the Agent's Secret names are recorded so an operator knows what must be
// re-created, never their values.
type Manifest struct {
	FormatVersion string    `json:"formatVersion"`
	ExportedAt    time.Time `json:"exportedAt"`
	Source        Source    `json:"source"`
	// Mounts inventories every filesystem visible to the source agent pod and
	// states whether it is in the archive, so nothing excluded is ever
	// presented as backed up.
	Mounts []Mount `json:"mounts"`
	// Excluded lists paths under the archived root that were deliberately
	// left out, each with a reason.
	Excluded []Exclusion `json:"excluded,omitempty"`
	Totals   Totals      `json:"totals"`
	// Entries must stay the last field: Write streams the manifest and
	// appends the entry list after everything else.
	Entries []Entry `json:"entries,omitempty"`
}

// Source identifies the agent the archive came from and carries the
// non-secret configuration a create-from-export flow needs.
type Source struct {
	Agent          string `json:"agent"`
	UID            string `json:"uid,omitempty"`
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
	Model          string `json:"model,omitempty"`
	Machine        string `json:"machine,omitempty"`
	KyberVersion   string `json:"kyberVersion,omitempty"`
	// PersistenceMode is the agent pod's root persistence layout (for example
	// "rootfs", where /persist/agentroot is the agent's whole root).
	PersistenceMode string `json:"persistenceMode,omitempty"`
	Disk            Disk   `json:"disk"`
	// LoginFiles are the harness login files, relative to a home directory,
	// that a restore never copies: a new agent always signs in with its own
	// Kyber-managed credential. Recorded so the archive describes them
	// wherever it is imported.
	LoginFiles []string `json:"loginFiles,omitempty"`
	// Config is the Agent spec with Secret values excluded by construction:
	// only names and non-secret settings are copied. An import applies it on
	// request; it is never authorization.
	Config *Config `json:"config,omitempty"`
}

// Disk records the source volume's characteristics.
type Disk struct {
	PVC           string   `json:"pvc"`
	StorageClass  string   `json:"storageClass,omitempty"`
	CapacityBytes int64    `json:"capacityBytes,omitempty"`
	RequestBytes  int64    `json:"requestBytes,omitempty"`
	AccessModes   []string `json:"accessModes,omitempty"`
	VolumeMode    string   `json:"volumeMode,omitempty"`
}

// Mount is one filesystem the source agent pod could see.
type Mount struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Archived bool   `json:"archived"`
	Reason   string `json:"reason"`
}

// Exclusion is a path under the archived root that was not archived.
type Exclusion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Totals summarises the archived entries.
type Totals struct {
	Entries  int   `json:"entries"`
	Files    int   `json:"files"`
	Dirs     int   `json:"dirs"`
	Symlinks int   `json:"symlinks"`
	Bytes    int64 `json:"bytes"`
}

// Entry is one archived filesystem object. Path is relative to the archived
// root, slash-separated, and never empty: the root itself is ".".
type Entry struct {
	Path    string    `json:"path"`
	Type    EntryType `json:"type"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	UID     int       `json:"uid"`
	GID     int       `json:"gid"`
	ModTime time.Time `json:"modTime"`
	Target  string    `json:"target,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
	// Image is set when the file is the base image's own copy, unchanged
	// since the image put it there (see ImageFile). Such a file is neither a
	// credential nor a crontab the agent installed.
	Image bool `json:"image,omitempty"`
}

// ZipName is the ZIP entry name for e.
func (e Entry) ZipName() string {
	if e.Path == "." {
		return PayloadPrefix
	}
	if e.Type == EntryDir {
		return PayloadPrefix + e.Path + "/"
	}
	return PayloadPrefix + e.Path
}

// CleanRelPath validates an archive-relative path and returns its canonical
// form. It rejects absolute paths, parent traversal, empty or dot segments,
// backslashes, and NUL bytes, so a path that passes can be joined under a root
// without escaping it lexically. (Symlink escapes are handled separately by
// extracting through os.Root and creating links last.)
func CleanRelPath(p string) (string, error) {
	if p == "." {
		return p, nil
	}
	if p == "" || !utf8.ValidString(p) || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, p)
	}
	// A backslash is an ordinary character in a Linux file name (systemd's
	// escaped unit names use them), so it is allowed. But some ZIP tools on
	// other systems treat it as a separator, so segments are also checked with
	// it as one: no archive can smuggle ".." past a Windows extractor.
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: %q", ErrUnsafePath, p)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "\\\\") || strings.HasSuffix(seg, "\\") {
			return "", fmt.Errorf("%w: %q", ErrUnsafePath, p)
		}
	}
	if path.Clean(p) != p {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, p)
	}
	return p, nil
}

// CheckVersion reports whether a manifest's format version is restorable.
func CheckVersion(version string) error {
	for _, v := range SupportedFormatVersions {
		if v == version {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnsupportedVersion, version)
}

// sensitiveSuffixes name well-known local credential files. They are
// archived like any other file (they are part of the disk), but a restore
// surfaces them so the operator decides whether the new agent should keep
// them; see SensitivePaths.
var sensitiveSuffixes = []string{
	"/.claude/.credentials.json",
	"/.codex/auth.json",
	"/.git-credentials",
	"/.netrc",
	"/.aws/credentials",
	"/.config/gh/hosts.yml",
	"/.docker/config.json",
	"/.kube/config",
	"/.npmrc",
	"/.pypirc",
}

// IsSensitive reports whether an entry is a regular file that looks like a
// local credential: the well-known files above, SSH private keys, and
// *.pem/*.key files. The image's own unchanged files are never credentials.
func IsSensitive(e Entry) bool {
	if e.Type != EntryFile || e.Image {
		return false
	}
	p := "/" + e.Path
	base := path.Base(p)
	if strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") ||
		strings.Contains(p, "/.ssh/") && strings.HasPrefix(base, "id_") && !strings.HasSuffix(base, ".pub") {
		return true
	}
	for _, s := range sensitiveSuffixes {
		if strings.HasSuffix(p, s) {
			return true
		}
	}
	return false
}

// IsLoginPath reports whether an archive path is one of the harness login
// files (home-relative, as in Source.LoginFiles).
func IsLoginPath(p string, loginFiles []string) bool {
	p = "/" + p
	for _, lf := range loginFiles {
		if lf != "" && strings.HasSuffix(p, "/"+strings.TrimPrefix(lf, "/")) {
			return true
		}
	}
	return false
}

// LoginPaths lists the regular files that are harness login files.
func LoginPaths(m *Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		if e.Type == EntryFile && IsLoginPath(e.Path, m.Source.LoginFiles) {
			out = append(out, e.Path)
		}
	}
	return out
}

// SensitivePaths lists the entries IsSensitive matches, other than harness
// login files (see LoginPaths).
func SensitivePaths(m *Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		if IsSensitive(e) && !IsLoginPath(e.Path, m.Source.LoginFiles) {
			out = append(out, e.Path)
		}
	}
	return out
}

// IsAgentCrontab reports whether an entry is a crontab the agent installed
// itself: a user crontab or an /etc/cron.d entry other than Kyber's own
// kyber-jobs, which the platform regenerates from the Agent spec at every
// boot. A restored copy of the disk would run it too.
func IsAgentCrontab(e Entry) bool {
	if e.Type != EntryFile || e.Size == 0 || e.Image {
		return false
	}
	p := "/" + e.Path
	switch {
	case strings.Contains(p, "/var/spool/cron/crontabs/"), strings.Contains(p, "/cron/crontabs/"):
		return true
	case (strings.Contains(p, "/etc/cron.d/") || strings.HasPrefix(p, "/cron/cron.d/")) && path.Base(p) != "kyber-jobs":
		return true
	}
	return false
}

// CronPaths lists the entries IsAgentCrontab matches.
func CronPaths(m *Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		if IsAgentCrontab(e) {
			out = append(out, e.Path)
		}
	}
	return out
}
