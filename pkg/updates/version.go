package updates

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a Kyber version: X.Y.Z, optionally with a pre-release suffix.
//
// Releases are always bare X.Y.Z (prepare-release.yml validates this before
// cutting a tag). Head-of-main builds carry a suffix — `1.0.1-25-gfd47d00`,
// the git-describe form — because the `main` channel needs to name and order
// every commit, not just tagged ones.
//
// Accepting the suffix does NOT relax the guard it was written for. That guard
// was about ranking across numbering eras (the OSS renumber left v2.x tags in
// GHCR beside the new v1.x, and anything ranking them naively picks v2.7.1
// over v1.0.1), and major/minor/patch still dominate every comparison. What
// keeps a pre-release out of a production cluster is the CHANNEL, not the
// parser: the stable channel discards any version with a suffix, so a cluster
// on stable can never be offered a main build. See Channel.Accepts.
type Version struct {
	Major, Minor, Patch int

	// Prerelease is everything after the first "-", with build metadata
	// (anything after "+") stripped — semver says metadata is ignored for
	// precedence, and we do the same.
	Prerelease string
}

// IsPrerelease reports whether this is anything other than a plain release.
func (v Version) IsPrerelease() bool { return v.Prerelease != "" }

// ParseVersion accepts "1.2.3", "v1.2.3", and the pre-release forms
// "1.2.3-<suffix>" and "1.2.3-<suffix>+<metadata>".
//
// The X.Y.Z part is still strict — three purely numeric components, nothing
// coerced. What is deliberately NOT enforced here is which versions a cluster
// may move to: that is the channel's job (Channel.Accepts), because the same
// string is legitimate on the canary and must never reach a production
// cluster. Enforcing it in the parser would mean the canary could not name its
// own version, which is exactly the bug that made the main channel impossible.
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if raw == "" {
		return Version{}, fmt.Errorf("empty version")
	}

	// Build metadata never affects precedence (semver §10), so drop it before
	// anything else looks at the string.
	if plus := strings.Index(raw, "+"); plus >= 0 {
		raw = raw[:plus]
	}
	var prerelease string
	if dash := strings.Index(raw, "-"); dash >= 0 {
		prerelease = raw[dash+1:]
		raw = raw[:dash]
		if prerelease == "" {
			return Version{}, fmt.Errorf("version %q has an empty pre-release suffix", s)
		}
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
	out.Prerelease = prerelease
	return out, nil
}

// String renders the bare form, without a leading "v".
func (v Version) String() string {
	if v.Prerelease != "" {
		return fmt.Sprintf("%d.%d.%d-%s", v.Major, v.Minor, v.Patch, v.Prerelease)
	}
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
	return comparePrerelease(v.Prerelease, other.Prerelease)
}

// comparePrerelease orders the suffix of two otherwise-equal versions.
//
// The suffix we actually produce is `git describe --tags` output —
// `1.0.2-3-ga1717c8` — and it means "3 commits PAST the v1.0.2 tag" (see
// build.yml, which stamps it). It is therefore strictly NEWER than 1.0.2, not
// older. Semver reads the same string as a pre-release OF 1.0.2 and so ranks
// it below, which is exactly backwards for this format.
//
// Getting that wrong is not cosmetic. It broke the main channel outright:
// ChartFeedClient.Latest picks the greatest accepted tag, so with 1.0.2 and
// 1.0.2-4-g47b6a7a both published, semver ordering elects the plain 1.0.2 and
// a canary tracking main is never offered a main build at all — while also
// being told to "upgrade" backwards onto the release it already contains.
// Caught on the canary 2026-08-13, which is what it exists for.
//
// So the offset is split off and compared numerically, and everything before
// it keeps strict semver ordering:
//
//	1.0.2-rc1 < 1.0.2 < 1.0.2-3-ga1717c8 < 1.0.2-4-g47b6a7a < 1.0.3
//	1.0.2-rc1 < 1.0.2-rc1-3-gabc1234 < 1.0.2
//
// Numeric comparison of the offset also fixes the lexical trap this code was
// already worried about — "9-gabc" must not outrank "10-gdef".
//
// What this does NOT change is which versions a cluster may take: that is the
// channel's job. A describe build still reports IsPrerelease, so the stable
// channel still discards it and a production cluster can never be handed a
// main build. See Channel.Accepts.
func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	aBase, aAhead := splitDescribe(a)
	bBase, bAhead := splitDescribe(b)
	if c := compareBaseSuffix(aBase, bBase); c != 0 {
		return c
	}
	// Same base: whichever sits more commits past it is newer. A plain
	// release, and any suffix that is not git-describe output, is zero
	// commits ahead — so 1.0.2 < 1.0.2-3-gabc1234 falls out of this.
	return sign(aAhead - bAhead)
}

// splitDescribe separates a trailing git-describe offset from the rest of the
// suffix, returning the remainder and the commit count. A suffix that is not
// describe output comes back unchanged with a zero count.
//
// The shape matched is `<base>-<n>-g<sha>` with the base optional, so both
// `3-ga1717c8` (describe from a release tag) and `rc1-3-gabc1234` (describe
// from a pre-release tag) are handled. The `g` prefix is git's own marker and
// the abbreviated sha is at least 7 hex digits, which is narrow enough that a
// deliberate pre-release identifier will not be mistaken for one.
func splitDescribe(pre string) (string, int) {
	gDash := strings.LastIndex(pre, "-g")
	if gDash < 0 {
		return pre, 0
	}
	sha := pre[gDash+2:]
	if !isAbbrevSHA(sha) {
		return pre, 0
	}
	head := pre[:gDash] // "<base>-<n>" or "<n>"
	nDash := strings.LastIndex(head, "-")
	countStr := head[nDash+1:] // nDash < 0 leaves the whole string
	ahead, err := strconv.Atoi(countStr)
	if err != nil || ahead < 0 || countStr == "" {
		return pre, 0
	}
	if nDash < 0 {
		return "", ahead
	}
	return head[:nDash], ahead
}

// isAbbrevSHA reports whether s is a git abbreviated object name.
func isAbbrevSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}

// compareBaseSuffix orders the part of two suffixes preceding any describe
// offset, under strict semver rules: an empty suffix is the release itself and
// outranks any pre-release of the same X.Y.Z.
func compareBaseSuffix(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	aParts, bParts := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if c := compareIdentifier(aParts[i], bParts[i]); c != 0 {
			return c
		}
	}
	// All shared identifiers equal: more identifiers ranks higher (semver §11).
	return sign(len(aParts) - len(bParts))
}

// compareIdentifier orders one dot-separated pre-release identifier, comparing
// a leading run of digits numerically so 10 outranks 9.
func compareIdentifier(a, b string) int {
	aNum, aRest := splitLeadingDigits(a)
	bNum, bRest := splitLeadingDigits(b)
	switch {
	case aNum >= 0 && bNum >= 0:
		if aNum != bNum {
			return sign(aNum - bNum)
		}
		return strings.Compare(aRest, bRest)
	case aNum >= 0:
		// Numeric identifiers rank below alphanumeric ones (semver §11).
		return -1
	case bNum >= 0:
		return 1
	}
	return strings.Compare(a, b)
}

// splitLeadingDigits returns the leading integer and the remainder, or -1 when
// the identifier does not start with a digit. Overlong digit runs return -1
// rather than overflowing.
func splitLeadingDigits(s string) (int, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return -1, s
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return -1, s
	}
	return n, s[i:]
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
