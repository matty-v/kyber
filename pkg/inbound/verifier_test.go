package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// computeMAC is a tiny test helper so tests don't depend on Verify's own MAC.
func computeMAC(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerify(t *testing.T) {
	secret := []byte("super-secret")
	body := []byte(`{"hello":"world"}`)
	good := computeMAC(secret, body)

	tests := []struct {
		name    string
		secret  []byte
		body    []byte
		header  string
		prefix  string
		wantErr error
	}{
		{
			name:    "matching signature with prefix",
			secret:  secret,
			body:    body,
			header:  "sha256=" + good,
			prefix:  "sha256=",
			wantErr: nil,
		},
		{
			name:    "matching signature bare",
			secret:  secret,
			body:    body,
			header:  good,
			prefix:  "",
			wantErr: nil,
		},
		{
			name:    "mismatched signature same length",
			secret:  secret,
			body:    body,
			header:  "sha256=" + reverseHex(good),
			prefix:  "sha256=",
			wantErr: ErrSignatureMismatch,
		},
		{
			name:    "wrong secret",
			secret:  secret,
			body:    body,
			header:  "sha256=" + computeMAC([]byte("nope"), body),
			prefix:  "sha256=",
			wantErr: ErrSignatureMismatch,
		},
		{
			name:    "missing prefix when expected",
			secret:  secret,
			body:    body,
			header:  good,
			prefix:  "sha256=",
			wantErr: ErrSignatureMismatch,
		},
		{
			name:    "bad hex",
			secret:  secret,
			body:    body,
			header:  "sha256=zzzz",
			prefix:  "sha256=",
			wantErr: ErrSignatureMismatch,
		},
		{
			name:    "empty header",
			secret:  secret,
			body:    body,
			header:  "",
			prefix:  "sha256=",
			wantErr: ErrSignatureMismatch,
		},
		{
			name:    "empty header no prefix",
			secret:  secret,
			body:    body,
			header:  "",
			prefix:  "",
			wantErr: ErrSignatureMismatch,
		},
		{
			name:    "empty secret still verifies if MACs match",
			secret:  []byte{},
			body:    body,
			header:  computeMAC([]byte{}, body),
			prefix:  "",
			wantErr: nil,
		},
		{
			name:    "empty body matches its own MAC",
			secret:  secret,
			body:    []byte{},
			header:  computeMAC(secret, []byte{}),
			prefix:  "",
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.secret, tt.body, tt.header, tt.prefix)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// reverseHex flips the byte order of a hex string so we get a same-length
// "wrong" MAC for the constant-time-compare test path.
func reverseHex(h string) string {
	b, _ := hex.DecodeString(h)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b)
}
