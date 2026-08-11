package updates

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a strict X.Y.Z release version. Kyber releases are always bare
// three-part semver (prepare-release.yml validates this before cutting a tag),
// so this deliberately does NOT accept pre-release or build-metadata suffixes:
// a version we cannot parse exactly is a version we refuse to upgrade to.
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion accepts "1.2.3" or "v1.2.3" and nothing else.
//
// Strictness is the point. The alternative — a permissive parser that coerces
// anything vaguely version-shaped — is how an update checker ends up proposing
// a "newer" version that is really a build label, a git-describe string, or a
// tag from a different numbering era. Kyber has already been bitten by the last
// of those: the OSS renumber left v2.x image tags in GHCR alongside the new
// v1.x, and anything ranking them naively picks v2.7.1 over v1.0.1. See
// kyber-deploy#140.
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if raw == "" {
		return Version{}, fmt.Errorf("empty version")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q is not strict X.Y.Z", s)
	}
	out := Version{}
	for i, p := range parts {
		if p == "" {
			return Version{}, fmt.Errorf("version %q has an empty component", s)
		}
		// Reject leading '+'/'-' and any non-digit run; strconv.Atoi would
		// happily accept "-1" and "+2".
		for _, r := range p {
			if r < '0' || r > '9' {
				return Version{}, fmt.Errorf("version %q has a non-numeric component %q", s, p)
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("version %q component %q: %w", s, p, err)
		}
		switch i {
		case 0:
			out.Major = n
		case 1:
			out.Minor = n
		case 2:
			out.Patch = n
		}
	}
	return out, nil
}

// String renders the bare form, without a leading "v".
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare returns -1 if v < other, 0 if equal, +1 if v > other.
func (v Version) Compare(other Version) int {
	switch {
	case v.Major != other.Major:
		return sign(v.Major - other.Major)
	case v.Minor != other.Minor:
		return sign(v.Minor - other.Minor)
	case v.Patch != other.Patch:
		return sign(v.Patch - other.Patch)
	}
	return 0
}

// GreaterThan reports whether v is strictly newer than other.
func (v Version) GreaterThan(other Version) bool { return v.Compare(other) > 0 }

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}
