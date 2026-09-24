package usersecrets

import (
	"errors"
	"strings"
	"testing"
)

// MAT-90 G13: file secrets had to be named like environment variables, so a
// certificate could not be stored as vault-cert.pem. A file key is now its own
// filename; kv keys keep the environment-variable grammar.
func TestKeyGrammarPerKind(t *testing.T) {
	for _, tc := range []struct {
		key            string
		kvErr, fileErr error
		fileName       string // expected mount name when the file key is valid
	}{
		{key: "vault-cert.pem", kvErr: ErrKeyBadGrammar, fileName: "vault-cert.pem"},
		{key: "id_ed25519", kvErr: ErrKeyBadGrammar, fileName: "id_ed25519"},
		{key: "Config.JSON", kvErr: ErrKeyBadGrammar, fileName: "Config.JSON"},
		{key: "APP_PEM", fileName: "app_pem.bin"}, // env-style: #75 mount path kept
		{key: "../x", kvErr: ErrKeyBadGrammar, fileErr: ErrFileKeyBadGrammar},
		{key: "a/b", kvErr: ErrKeyBadGrammar, fileErr: ErrFileKeyBadGrammar},
		{key: "a..b", kvErr: ErrKeyBadGrammar, fileErr: ErrFileKeyBadGrammar},
		{key: ".hidden", kvErr: ErrKeyBadGrammar, fileErr: ErrFileKeyBadGrammar},
		{key: "-x", kvErr: ErrKeyBadGrammar, fileErr: ErrFileKeyBadGrammar},
		{key: "has space", kvErr: ErrKeyBadGrammar, fileErr: ErrFileKeyBadGrammar},
		{key: "", kvErr: ErrKeyEmpty, fileErr: ErrKeyEmpty},
		{key: strings.Repeat("a", MaxFileKeyLength), kvErr: ErrKeyTooLong, fileName: strings.Repeat("a", MaxFileKeyLength)},
		{key: strings.Repeat("a", MaxFileKeyLength+1), kvErr: ErrKeyTooLong, fileErr: ErrKeyTooLong},
		{key: "KYBER_TOKEN", kvErr: ErrKeyReservedPrefix, fileErr: ErrKeyReservedPrefix},
		// Would share /user-secrets/app_pem.bin with the key APP_PEM.
		{key: "app_pem.bin", kvErr: ErrKeyBadGrammar, fileErr: ErrFileKeyLegacyForm},
		{key: "App_Pem.bin", kvErr: ErrKeyBadGrammar, fileName: "App_Pem.bin"},
	} {
		if err := ValidateKey(tc.key); !errors.Is(err, tc.kvErr) {
			t.Errorf("ValidateKey(%q) = %v, want %v", tc.key, err, tc.kvErr)
		}
		err := ValidateFileKey(tc.key)
		if !errors.Is(err, tc.fileErr) {
			t.Errorf("ValidateFileKey(%q) = %v, want %v", tc.key, err, tc.fileErr)
		}
		if err != nil {
			continue
		}
		if got := FileName(tc.key); got != tc.fileName {
			t.Errorf("FileName(%q) = %q, want %q", tc.key, got, tc.fileName)
		}
		if got := FileKey(FileName(tc.key)); got != tc.key {
			t.Errorf("FileKey(FileName(%q)) = %q, want the key back", tc.key, got)
		}
	}
}

// Every valid key names exactly one file, so two keys can never share a mount
// path and a listing always maps a file back to one key.
func TestFileNamesAreUniqueAcrossKeyStyles(t *testing.T) {
	seen := map[string]string{}
	for _, key := range []string{"APP_PEM", "app_pem", "app_pem.pem", "App_Pem.bin", "APP_PEM2", "vault-cert.pem"} {
		if ValidateFileKey(key) != nil {
			continue
		}
		name := FileName(key)
		if other, dup := seen[name]; dup {
			t.Errorf("keys %q and %q both mount at %s", other, key, name)
		}
		seen[name] = key
	}
	if FileKey("notes.txt..") != "" || FileKey("../etc") != "" {
		t.Error("FileKey accepted a hand-edited name no valid key produces")
	}
}
