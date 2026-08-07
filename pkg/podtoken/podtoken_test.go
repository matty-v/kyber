package podtoken

import (
	"strings"
	"testing"
)

func TestSignParseRoundTrip(t *testing.T) {
	key := []byte("test-cluster-signing-key-0123456789")
	for _, identity := range []string{"han", "luke", NodeAgentIdentity, "a"} {
		tok := Sign(identity, key)
		if tok == "" {
			t.Fatalf("Sign(%q) returned empty token", identity)
		}
		got, err := Parse(tok, key)
		if err != nil {
			t.Fatalf("Parse(Sign(%q)) error: %v", identity, err)
		}
		if got != identity {
			t.Fatalf("round-trip identity = %q, want %q", got, identity)
		}
	}
}

func TestParseRejectsWrongKey(t *testing.T) {
	tok := Sign("han", []byte("the-real-signing-key-aaaaaaaaaaaa"))
	if _, err := Parse(tok, []byte("a-different-signing-key-bbbbbbbb")); err == nil {
		t.Fatal("Parse accepted a token signed with a different key")
	}
}

func TestParseRejectsTamperedIdentity(t *testing.T) {
	key := []byte("test-cluster-signing-key-0123456789")
	// Forge a token claiming identity "luke" but with han's signature: take
	// han's valid token, swap the identity segment for luke's encoding.
	hanTok := Sign("han", key)
	lukeTok := Sign("luke", key)
	hanSig := hanTok[strings.IndexByte(hanTok, '.'):]
	lukePayload := lukeTok[:strings.IndexByte(lukeTok, '.')]
	forged := lukePayload + hanSig
	if _, err := Parse(forged, key); err == nil {
		t.Fatal("Parse accepted a token with a forged identity (sig mismatch)")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	key := []byte("test-cluster-signing-key-0123456789")
	cases := map[string]string{
		"empty":         "",
		"no-dot":        "abcdef",
		"too-many-dots": "a.b.c",
		"bad-b64":       "!!!.???",
		"empty-segs":    ".",
	}
	for name, tok := range cases {
		if _, err := Parse(tok, key); err == nil {
			t.Errorf("%s: Parse(%q) accepted a malformed token", name, tok)
		}
	}
}

func TestParseRejectsEmptyKey(t *testing.T) {
	tok := Sign("han", []byte("real-key-xxxxxxxxxxxxxxxxxxxxxxxx"))
	if _, err := Parse(tok, nil); err == nil {
		t.Fatal("Parse with an empty key must fail closed")
	}
}

func TestParseIsDeterministicForSameInput(t *testing.T) {
	key := []byte("test-cluster-signing-key-0123456789")
	if Sign("han", key) != Sign("han", key) {
		t.Fatal("Sign is not deterministic for identical (identity, key)")
	}
}
