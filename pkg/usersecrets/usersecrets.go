// Package usersecrets holds pure validation for the user-defined per-agent
// secrets feature (see docs/2026-04-18-user-secrets-design.md and issue #75).
//
// The API layer calls these validators before any Kubernetes write. They have
// no k8s dependency and are safe to exercise from unit tests without a fake
// client.
package usersecrets

import (
	"errors"
	"regexp"
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

var keyGrammar = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Validation errors. These are typed so the API layer can map each to a
// specific 400 error code without string-matching.
var (
	ErrKeyEmpty          = errors.New("usersecrets: key must not be empty")
	ErrKeyTooLong        = errors.New("usersecrets: key exceeds maximum length")
	ErrKeyBadGrammar     = errors.New("usersecrets: key must match [A-Z][A-Z0-9_]*")
	ErrKeyReservedPrefix = errors.New("usersecrets: key uses a reserved prefix (KYBER_ or USER_)")
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
