package api

// Browser-session credentials are SIGNED, not stored.
//
// The previous design kept live sessions in a map on the authenticator. That
// map was process-local, so every control-plane restart — upgrade, node drain,
// OOM, redeploy — silently signed out every operator's browser, and it was
// capped at 256 entries with arbitrary eviction on overflow. Operators were
// re-pasting their API key far more often than the TTL implied (MAT-38).
//
// A signed token needs no server-side state: the cookie carries the caller and
// its expiry, and the control plane verifies an HMAC over both. Restarting the
// process no longer signs anyone out, and there is no session cap to overflow.
//
// The signing key is DERIVED FROM THE LIVE API KEY rather than stored
// separately. That is deliberate, and does three jobs at once:
//
//   - No new storage, so this behaves identically in a cluster install and in
//     a standalone install with no Secret available to write to.
//   - It survives a restart for free, because the API key itself is read from
//     the environment / Secret at process start.
//   - Rotating the API key changes the derived key, which invalidates every
//     outstanding cookie — preserving exactly what the old
//     ClearBrowserSessions did (see routes_rotate_api_key.go).

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// browserSessionTokenVersion prefixes the token AND is covered by the
	// signature, so a future v2 layout cannot be replayed as a v1 one.
	browserSessionTokenVersion = "v1"
	// browserSessionKeyInfo is the HKDF info string. Domain separation: the
	// derived signing key must not collide with anything else the API key
	// might ever seed.
	browserSessionKeyInfo = "kyber-browser-session-v1"
	// browserSessionKeyLen is the HMAC-SHA256 key length in bytes.
	browserSessionKeyLen = 32
)

// errNoBrowserSessionKey is returned when there is no API key to derive from.
// Fail closed: an empty key would derive a constant, publicly computable
// signing key, which would let anyone mint a full-scope session.
var errNoBrowserSessionKey = errors.New("browser sessions unavailable: no API key is configured")

// errMalformedBrowserSession covers every structural rejection — bad version,
// wrong segment count, undecodable base64, unparseable claims. They share one
// error because the caller's response is identical and distinguishing them for
// an unauthenticated client would only describe the token format to an
// attacker.
var errMalformedBrowserSession = errors.New("malformed browser session token")

// browserSessionClaims is the token payload. Field names are single letters
// because this round-trips through a cookie header on every request.
//
// The claims deliberately do NOT carry the caller's scopes. A token names WHO
// the session is for; what that principal may do is resolved against live
// configuration on every request (APIKeyAuthenticator.callerForSession). If
// the scopes were baked in, removing a scoped caller from configuration would
// leave their browser holding its old authority until the token expired —
// which the old server-side session map avoided only by accident, because a
// restart wiped every session.
//
// The payload is signed, not encrypted: it is readable by anyone holding the
// cookie. That is fine — a caller name and an expiry are not secrets, and the
// cookie is HttpOnly + SameSite=Strict.
type browserSessionClaims struct {
	Name      string `json:"n"`
	FullScope bool   `json:"f,omitempty"`
	// KeyBinding ties a scoped caller's session to the key it was issued
	// against, so replacing that key in configuration ends the session. Empty
	// for full-scope sessions, which are already bound to the shared key by
	// the signature itself. See browserSessionKeyBinding.
	KeyBinding string `json:"kb,omitempty"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
}

// expiresAt is the moment the session stops being valid.
func (c browserSessionClaims) expiresAt() time.Time { return time.Unix(c.ExpiresAt, 0) }

// browserSessionSigningKey derives the HMAC key for the given API key.
func browserSessionSigningKey(apiKey string) ([]byte, error) {
	if apiKey == "" {
		return nil, errNoBrowserSessionKey
	}
	key, err := hkdf.Key(sha256.New, []byte(apiKey), nil, browserSessionKeyInfo, browserSessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("deriving browser session key: %w", err)
	}
	return key, nil
}

// browserSessionKeyBinding is an opaque, unforgeable stand-in for a scoped
// caller's key: HMAC of the key under the session signing key, truncated.
//
// It exists so that replacing a caller's key value in configuration ends that
// caller's browser sessions. Without it, sessions resolve by NAME alone, and
// the natural revocation move — swap the leaked key, keep the entry — would
// leave a compromised cookie working until it expired.
//
// Keyed rather than a plain digest so the claim cannot be used to brute-force
// the caller key offline, and truncated because it only ever needs to be
// compared, never inverted.
func browserSessionKeyBinding(apiKey, callerKey string) (string, error) {
	key, err := browserSessionSigningKey(apiKey)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(browserSessionMAC(key, "caller-key:"+callerKey)[:16]), nil
}

// signBrowserSession encodes caller into a signed token valid for ttl from
// issuedAt. Output layout: "<version>.<base64url(claims)>.<base64url(hmac)>",
// where the HMAC covers "<version>.<base64url(claims)>".
func signBrowserSession(apiKey string, caller Caller, keyBinding string, issuedAt time.Time, ttl time.Duration) (string, error) {
	key, err := browserSessionSigningKey(apiKey)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(browserSessionClaims{
		Name:       caller.Name,
		FullScope:  caller.Scopes.full,
		KeyBinding: keyBinding,
		IssuedAt:   issuedAt.Unix(),
		ExpiresAt:  issuedAt.Add(ttl).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("encoding browser session claims: %w", err)
	}
	signed := browserSessionTokenVersion + "." + base64.RawURLEncoding.EncodeToString(raw)
	return signed + "." + base64.RawURLEncoding.EncodeToString(browserSessionMAC(key, signed)), nil
}

// verifyBrowserSession authenticates a token and returns the claims it
// carries. The signature is checked BEFORE the claims are parsed, so
// unverified bytes never reach the JSON decoder.
//
// This establishes only that the control plane issued the token and that it
// has not expired. Turning the claims into an authorized Caller is a separate
// step against live configuration — see callerForSession.
func verifyBrowserSession(apiKey, token string, now time.Time) (*browserSessionClaims, error) {
	key, err := browserSessionSigningKey(apiKey)
	if err != nil {
		return nil, err
	}

	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != browserSessionTokenVersion {
		return nil, errMalformedBrowserSession
	}
	encodedClaims, encodedMAC, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(encodedMAC, ".") {
		return nil, errMalformedBrowserSession
	}
	presentedMAC, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return nil, errMalformedBrowserSession
	}
	if !hmac.Equal(presentedMAC, browserSessionMAC(key, version+"."+encodedClaims)) {
		return nil, errors.New("browser session signature does not verify")
	}

	raw, err := base64.RawURLEncoding.DecodeString(encodedClaims)
	if err != nil {
		return nil, errMalformedBrowserSession
	}
	var claims browserSessionClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, errMalformedBrowserSession
	}
	if !now.Before(claims.expiresAt()) {
		return nil, errors.New("browser session expired")
	}
	return &claims, nil
}

func browserSessionMAC(key []byte, signed string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signed))
	return mac.Sum(nil)
}
