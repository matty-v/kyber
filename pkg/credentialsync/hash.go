// Package credentialsync defines the private ordering identifiers used when a
// runtime persists provider-refreshed credentials back to its Kubernetes
// Secret. Hashes are comparisons only: they are never credentials and must not
// be used as authentication tokens.
package credentialsync

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"
)

// HashClaude returns a stable identifier for the complete Claude OAuth
// credential. NUL separators are unambiguous because environment variables and
// JSON strings cannot carry a literal NUL through this integration boundary.
func HashClaude(accessToken, refreshToken string, expiresAt int64) string {
	h := sha256.New()
	writeField(h, accessToken)
	writeField(h, refreshToken)
	writeField(h, strconv.FormatInt(expiresAt, 10))
	return hex.EncodeToString(h.Sum(nil))
}

// HashOpaque returns the identifier for a provider-owned opaque credential
// document, such as Codex auth.json.
func HashOpaque(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ValidHash reports whether value is a canonical lowercase SHA-256 hex digest.
func ValidHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	return hex.EncodeToString(decoded) == value
}

func writeField(h hash.Hash, value string) {
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{0})
}
