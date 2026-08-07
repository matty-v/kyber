// Package podtoken implements the control-plane-signed pod-token used to
// authenticate callers of the cluster-internal API (:8082) — kyber#566,
// Option A of Obi-wan's design.
//
// A pod-token is a compact, stateless bearer credential that binds a single
// identity (an agent name, or the reserved node-agent identity) to the cluster
// signing key via HMAC-SHA256. The control plane mints one per agent (the
// reconciler) and one for the node-agent (control-plane startup); the internal
// API verifies it on every request and enforces act-on-self-only. Validation is
// O(1) with no Kubernetes round-trip — the identity is recovered straight from
// the token and the HMAC proves it was minted by the holder of the cluster key.
//
// This is a leaf package on purpose: pkg/api (verify) already imports
// pkg/controllers/agent, and the reconciler (sign) imports pkg/api/v1, so the
// shared sign/parse primitive cannot live in pkg/api without creating an import
// cycle. Both sides depend on this package instead.
//
// Token wire format:
//
//	base64url(identity) "." base64url(HMAC_SHA256(key, identity))
//
// base64url (RawURLEncoding, unpadded) keeps the token header-safe so clients
// can send it verbatim as `Authorization: Bearer <token>`.
package podtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// NodeAgentIdentity is the reserved identity carried by the node-agent's
// pod-token. The internal API admits this identity (and only this identity) to
// the /internal/machines/* and /internal/nodes/* routes; it is never a valid
// caller on an /internal/agents/{name}/* route. Agent names are Kubernetes
// object names and so can never collide with this value (it contains no
// characters an Agent name is built from beyond the hyphen, and no Agent is
// named "node-agent" in practice — but the machine/node routes gate on it
// explicitly regardless).
const NodeAgentIdentity = "node-agent"

// ErrInvalidToken is returned by Parse for any token that is malformed, signed
// with a different key, or tampered with. The error is deliberately opaque — it
// never distinguishes "wrong key" from "tampered" from "malformed" so a caller
// cannot use it as an oracle.
var ErrInvalidToken = errors.New("invalid pod token")

var enc = base64.RawURLEncoding

// Sign mints a pod-token for identity using key. The same (identity, key) pair
// always produces the same token (stateless, cacheable on the client). key must
// be non-empty; signing with an empty key yields a token that Parse will reject.
func Sign(identity string, key []byte) string {
	mac := computeMAC(identity, key)
	return enc.EncodeToString([]byte(identity)) + "." + enc.EncodeToString(mac)
}

// Parse verifies token against key and returns the embedded identity. It fails
// closed (ErrInvalidToken) on an empty key, a malformed token, or any signature
// mismatch. The HMAC comparison is constant-time.
func Parse(token string, key []byte) (string, error) {
	if len(key) == 0 {
		return "", ErrInvalidToken
	}
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		// Missing separator, empty payload, or empty signature segment.
		return "", ErrInvalidToken
	}
	payload, sig := token[:dot], token[dot+1:]
	if strings.IndexByte(sig, '.') != -1 {
		// More than two segments.
		return "", ErrInvalidToken
	}
	identityBytes, err := enc.DecodeString(payload)
	if err != nil {
		return "", ErrInvalidToken
	}
	presentedMAC, err := enc.DecodeString(sig)
	if err != nil {
		return "", ErrInvalidToken
	}
	identity := string(identityBytes)
	if !hmac.Equal(presentedMAC, computeMAC(identity, key)) {
		return "", ErrInvalidToken
	}
	return identity, nil
}

// computeMAC returns HMAC-SHA256(key, identity).
func computeMAC(identity string, key []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(identity))
	return h.Sum(nil)
}
