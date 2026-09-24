// Package usersecrets holds pure validation for the user-defined per-agent
// secrets feature (see docs/design/2026-04-18-user-secrets-design.md and issue #75).
//
// The API layer calls these validators before any Kubernetes write. They have
// no k8s dependency and are safe to exercise from unit tests without a fake
// client.
package usersecrets

import (
	"errors"
	"regexp"
	"strings"
)

// Size limits, per issue #75.
const (
	// MaxEntryBytes is the per-entry size limit. Applies to both kv values and file bytes.
	MaxEntryBytes = 64 * 1024
	// MaxAggregateBytes is the total across all entries (both kinds combined) for a single agent.
	MaxAggregateBytes = 256 * 1024
	// MaxKeyLength is the grammar upper bound on key length in characters.
	MaxKeyLength = 64
)

// Reserved key prefixes, per issue #75. KYBER_ is reserved for framework-
// supplied env vars; USER_ is the mount-time prefix the controller applies to
// kv entries, so operator-supplied keys must not already start with it.
var reservedPrefixes = []string{"KYBER_", "USER_"}

// keyGrammar is the kv (environment variable) grammar.
var keyGrammar = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// fileKeyGrammar is the grammar for a file key that is its own filename under
// /user-secrets: a Kubernetes Secret data key with no path separator and no
// leading dot. MaxFileKeyLength bounds it at the Secret key limit.
var fileKeyGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// MaxFileKeyLength is the upper bound on a filename-style file key.
const MaxFileKeyLength = 253

// legacyFileSuffix is appended to the lowercased key of an env-style file key
// (#75: FOO mounts at /user-secrets/foo.bin). Kept so existing entries and the
// agents that read them keep their paths.
const legacyFileSuffix = ".bin"

// Validation errors. These are typed so the API layer can map each to a
// specific 400 error code without string-matching.
var (
	ErrKeyEmpty          = errors.New("usersecrets: key must not be empty")
	ErrKeyTooLong        = errors.New("usersecrets: key exceeds maximum length")
	ErrKeyBadGrammar     = errors.New("usersecrets: key must match [A-Z][A-Z0-9_]*")
	ErrKeyReservedPrefix = errors.New("usersecrets: key uses a reserved prefix (KYBER_ or USER_)")
	ErrFileKeyBadGrammar = errors.New("usersecrets: file key must be a filename: [A-Za-z0-9] then [A-Za-z0-9._-], at most 253 characters, with no path and no \"..\"")
	ErrFileKeyLegacyForm = errors.New("usersecrets: a lowercase name.bin file key is the mount name of the key NAME; use NAME instead")
	ErrValueTooLarge     = errors.New("usersecrets: value exceeds per-entry size limit")
	ErrAggregateTooLarge = errors.New("usersecrets: aggregate size exceeds per-agent limit")
)

// ValidateKey enforces the key grammar and reserved-prefix rules from #75.
// Length is checked before the regex so ErrKeyTooLong fires on oversize keys
// rather than the catch-all ErrKeyBadGrammar.
func ValidateKey(key string) error {
	if key == "" {
		return ErrKeyEmpty
	}
	if len(key) > MaxKeyLength {
		return ErrKeyTooLong
	}
	if !keyGrammar.MatchString(key) {
		return ErrKeyBadGrammar
	}
	for _, p := range reservedPrefixes {
		if len(key) >= len(p) && key[:len(p)] == p {
			return ErrKeyReservedPrefix
		}
	}
	return nil
}

// ValidateFileKey enforces the file-key rules. A file key is either an
// env-style key (validated exactly as ValidateKey, mounted as
// /user-secrets/<lowercase>.bin for compatibility with #75) or a filename such
// as vault-cert.pem, mounted under that name. Every valid kv key is therefore a
// valid file key, which makes this the check for a request whose kind is not
// yet known.
//
// A lowercase "name.bin" filename is refused: it is the mount name the
// env-style key NAME already produces, and accepting it would make two keys
// share one file.
func ValidateFileKey(key string) error {
	if key == "" {
		return ErrKeyEmpty
	}
	if keyGrammar.MatchString(key) {
		return ValidateKey(key)
	}
	if len(key) > MaxFileKeyLength {
		return ErrKeyTooLong
	}
	if !fileKeyGrammar.MatchString(key) || strings.Contains(key, "..") {
		return ErrFileKeyBadGrammar
	}
	if legacyKeyFromFileName(key) != "" {
		return ErrFileKeyLegacyForm
	}
	return nil
}

// FileName returns the filename under /user-secrets (and the Secret data key)
// for a valid file key.
func FileName(key string) string {
	if keyGrammar.MatchString(key) {
		return strings.ToLower(key) + legacyFileSuffix
	}
	return key
}

// FileKey inverts FileName. It returns "" for a name FileName never produces,
// such as one hand-edited into the Secret.
func FileKey(name string) string {
	if key := legacyKeyFromFileName(name); key != "" {
		return key
	}
	if keyGrammar.MatchString(name) || !fileKeyGrammar.MatchString(name) || strings.Contains(name, "..") {
		return ""
	}
	return name
}

func legacyKeyFromFileName(name string) string {
	stem, ok := strings.CutSuffix(name, legacyFileSuffix)
	if !ok || stem != strings.ToLower(stem) {
		return ""
	}
	key := strings.ToUpper(stem)
	if len(key) > MaxKeyLength || !keyGrammar.MatchString(key) {
		return ""
	}
	return key
}

// ValidateEntrySize checks that a single entry's byte size is within the per-entry limit.
// Used for both kv values and file bytes — the limit is the same for both kinds.
func ValidateEntrySize(n int) error {
	if n > MaxEntryBytes {
		return ErrValueTooLarge
	}
	return nil
}

// ValidateAggregate checks that the proposed total size across all entries for
// an agent is within the aggregate limit. Caller computes the total — including
// the size of the entry being added or replaced — and passes it here.
func ValidateAggregate(totalBytes int) error {
	if totalBytes > MaxAggregateBytes {
		return ErrAggregateTooLarge
	}
	return nil
}
