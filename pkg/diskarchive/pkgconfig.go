package diskarchive

import (
	"bufio"
	"bytes"
	"path"
	"strings"
)

// maxPackageConfigBytes bounds the package-manager config files whose content
// is checked; a larger one is flagged by name as before.
const maxPackageConfigBytes = 64 << 10

// isPackageConfig reports the package-manager config files that may or may
// not hold credentials.
func isPackageConfig(p string) bool {
	switch path.Base(p) {
	case ".npmrc", ".pypirc":
		return true
	}
	return false
}

// packageConfigHasCredentials reports whether an .npmrc or .pypirc sets any
// auth field: npm's _auth, _authToken, _password and username (scoped as
// //registry/:_authToken=… or not), or pypirc's password. Comments and
// sections are ignored; anything unparseable counts as a credential.
func packageConfigHasCredentials(b []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			key, _, ok = strings.Cut(line, ":")
			if !ok {
				continue
			}
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if i := strings.LastIndex(key, ":"); i >= 0 { // //registry.example/:_authToken
			key = key[i+1:]
		}
		switch key {
		case "_auth", "_authtoken", "_password", "username", "password":
			return true
		}
	}
	return sc.Err() != nil
}
