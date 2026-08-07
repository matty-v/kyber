// Package inbound contains pure-logic primitives that back the inbound
// prompts feature (kyber#208): HMAC verification, body-hash dedupe, request
// dispatch decisioning, per-binding rate limiting, and per-agent in-flight
// queues. None of this code performs HTTP, Kubernetes, or subprocess I/O —
// callers wire these primitives into the API server and controllers.
package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrSignatureMismatch is returned by Verify whenever the computed HMAC does
// not match the supplied header value, the prefix is wrong, or the hex is
// malformed. Callers MUST treat this as a hard authentication failure and
// MUST NOT differentiate among its causes externally.
var ErrSignatureMismatch = errors.New("inbound: signature mismatch")

// Verify checks that signatureHeaderValue is HMAC-SHA256(body, secret)
// hex-encoded, with the supplied prefix stripped before decoding. The
// comparison is performed in constant time. Any failure (missing prefix,
// malformed hex, mismatched MAC) collapses to ErrSignatureMismatch.
//
// An empty prefix is permitted; in that case the entire header value is
// treated as the hex MAC. An empty header value is always a mismatch (even if
// the prefix is also empty and the body is empty), because callers should
// not be allowed to suppress signature checks by sending no header.
func Verify(secret []byte, body []byte, signatureHeaderValue string, prefix string) error {
	if signatureHeaderValue == "" {
		return ErrSignatureMismatch
	}
	if prefix != "" {
		if !strings.HasPrefix(signatureHeaderValue, prefix) {
			return ErrSignatureMismatch
		}
		signatureHeaderValue = signatureHeaderValue[len(prefix):]
	}
	got, err := hex.DecodeString(signatureHeaderValue)
	if err != nil {
		return ErrSignatureMismatch
	}

	mac := hmac.New(sha256.New, secret)
	// hash.Hash.Write never returns an error.
	_, _ = mac.Write(body)
	want := mac.Sum(nil)

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrSignatureMismatch
	}
	return nil
}
