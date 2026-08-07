package api

import (
	"regexp"
	"strings"
)

// validNameRe matches names that are safe for Kubernetes resource names and DNS subdomains:
// lowercase alpha start, then 0-62 lowercase alphanumeric or hyphen characters.
var validNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// isValidName reports whether name is a valid Kubernetes resource / DNS-safe name.
func isValidName(name string) bool {
	return validNameRe.MatchString(name)
}

// trimPrefix strips prefix from path, returning ("", false) if the path does not start with prefix.
func trimPrefix(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}

// splitAction splits a suffix like "{name}" or "{name}/action" into the resource name and
// optional sub-action. Returns ("", "", false) when the suffix is empty or malformed.
//
// Example inputs and outputs:
//
//	"dave"           → ("dave", "", true)
//	"dave/start"     → ("dave", "start", true)
//	""               → ("", "", false)
//	"/"              → ("", "", false)
func splitAction(suffix string) (name, action string, ok bool) {
	suffix = strings.TrimPrefix(suffix, "/")
	if suffix == "" {
		return "", "", false
	}
	parts := strings.SplitN(suffix, "/", 2)
	name = parts[0]
	if name == "" {
		return "", "", false
	}
	if len(parts) == 2 {
		action = parts[1]
	}
	return name, action, true
}
