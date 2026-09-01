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
type browserSessionClaims struct {
	Name      string   `json:"n"`
	FullScope bool     `json:"f,omitempty"`
	Scopes    []string `json:"s,omitempty"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
}

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

// signBrowserSession encodes caller into a signed token valid for ttl from
// issuedAt. Output layout: "<version>.<base64url(claims)>.<base64url(hmac)>",
// where the HMAC covers "<version>.<base64url(claims)>".
func signBrowserSession(apiKey string, caller Caller, issuedAt time.Time, ttl time.Duration) (string, error) {
	key, err := browserSessionSigningKey(apiKey)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(browserSessionClaims{
		Name:      caller.Name,
		FullScope: caller.Scopes.full,
		Scopes:    caller.Scopes.names(),
		IssuedAt:  issuedAt.Unix(),
		ExpiresAt: issuedAt.Add(ttl).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("encoding browser session claims: %w", err)
	}
	signed := browserSessionTokenVersion + "." + base64.RawURLEncoding.EncodeToString(raw)
	return signed + "." + base64.RawURLEncoding.EncodeToString(browserSessionMAC(key, signed)), nil
}

// verifyBrowserSession authenticates a token and returns the caller it names
// plus its expiry. The signature is checked BEFORE the claims are parsed, so
// unverified bytes never reach the JSON decoder.
func verifyBrowserSession(apiKey, token string, now time.Time) (*Caller, time.Time, error) {
	key, err := browserSessionSigningKey(apiKey)
	if err != nil {
		return nil, time.Time{}, err
	}

	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != browserSessionTokenVersion {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	encodedClaims, encodedMAC, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(encodedMAC, ".") {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	presentedMAC, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	if !hmac.Equal(presentedMAC, browserSessionMAC(key, version+"."+encodedClaims)) {
		return nil, time.Time{}, errors.New("browser session signature does not verify")
	}

	raw, err := base64.RawURLEncoding.DecodeString(encodedClaims)
	if err != nil {
		return nil, time.Time{}, errMalformedBrowserSession
	}
	var claims browserSessionClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, time.Time{}, errMalformedBrowserSession
	}

	expiresAt := time.Unix(claims.ExpiresAt, 0)
	if !now.Before(expiresAt) {
		return nil, time.Time{}, errors.New("browser session expired")
	}

	caller := &Caller{Name: claims.Name}
	if claims.FullScope {
		caller.Scopes = newFullScopeSet()
	} else {
		scopes := make([]Scope, 0, len(claims.Scopes))
		for _, s := range claims.Scopes {
			scopes = append(scopes, Scope(s))
		}
		caller.Scopes = newScopeSet(scopes...)
	}
	return caller, expiresAt, nil
}

func browserSessionMAC(key []byte, signed string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signed))
	return mac.Sum(nil)
}
