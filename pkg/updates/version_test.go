package updates

import "testing"

func TestParseVersion_AcceptsStrictSemver(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Version
	}{
		{"1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{"v1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{" v0.0.1 ", Version{Patch: 1}},
		{"10.20.30", Version{Major: 10, Minor: 20, Patch: 30}},
	} {
		got, err := ParseVersion(tc.in)
		if err != nil {
			t.Errorf("ParseVersion(%q) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// The X.Y.Z part stays strict: each of these is a string a permissive parser
// would coerce into a plausible-looking version, which is how an update checker
// ends up offering a build label as if it were a release.
//
// Note what is NOT here any more: pre-release forms like "1.2.3-rc1" and the
// git-describe "2.1.1-7-gd64fbbd" now PARSE, because the main channel has to be
// able to name head-of-main builds. What keeps them away from a production
// cluster is Channel.Accepts, covered below — moving that guard from the parser
// to the channel is the whole point, and TestChannelAccepts is where the safety
// property now lives.
func TestParseVersion_RejectsAnythingElse(t *testing.T) {
	for _, in := range []string{
		"",
		"v",
		"1.2",
		"1.2.3.4",
		"1.2.x",
		"latest",
		"-1.2.3",
		"1.-2.3",
		"1..3",
	} {
		if v, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) = %+v, want an error", in, v)
		}
	}
}

func TestVersion_CompareOrdersByPrecedence(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.0.9", "1.1.0", -1},
		// Double-digit components must compare numerically, not
		// lexicographically — "1.10.0" > "1.9.0" is the classic string-compare
		// bug and it would stall a cluster on the older release.
		{"1.10.0", "1.9.0", 1},
		{"1.0.10", "1.0.9", 1},
	} {
		a, err := ParseVersion(tc.a)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.a, err)
		}
		b, err := ParseVersion(tc.b)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.b, err)
		}
		if got := a.Compare(b); got != tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := a.GreaterThan(b); got != (tc.want > 0) {
			t.Errorf("%s.GreaterThan(%s) = %v, want %v", tc.a, tc.b, got, tc.want > 0)
		}
	}
}

// The renumber hazard, pinned. GHCR still holds v2.7.x images from before the
// OSS clean-start at v1.0.0. Pure version ordering says 2.7.1 > 1.0.1, so
// anything that ranks tags itself will happily propose rolling a cluster
// BACKWARDS onto pre-renumber artifacts. This test documents that the
// comparator alone is not a safe source of "what should I upgrade to" — the
// feed's explicit "latest" marker is (see FeedClient.Latest).
func TestVersion_ComparatorAloneWouldPreferPreRenumberVersions(t *testing.T) {
	old, err := ParseVersion("2.7.1")
	if err != nil {
		t.Fatal(err)
	}
	current, err := ParseVersion("1.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !old.GreaterThan(current) {
		t.Fatal("expected 2.7.1 > 1.0.1 by pure version ordering — if this ever fails, the comment above and FeedClient's rationale need revisiting")
	}
}

func TestVersion_StringRoundTrips(t *testing.T) {
	for _, in := range []string{"0.0.0", "1.2.3", "10.20.30"} {
		v, err := ParseVersion(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got := v.String(); got != in {
			t.Errorf("ParseVersion(%q).String() = %q, want %q", in, got, in)
		}
	}
}

func TestParseVersion_AcceptsPrereleases(t *testing.T) {
	for _, tc := range []struct {
		in         string
		want       Version
		wantString string
	}{
		// The git-describe form every head-of-main build carries.
		{"1.0.1-25-gfd47d00", Version{Major: 1, Minor: 0, Patch: 1, Prerelease: "25-gfd47d00"}, "1.0.1-25-gfd47d00"},
		{"v1.0.1-25-gfd47d00", Version{Major: 1, Minor: 0, Patch: 1, Prerelease: "25-gfd47d00"}, "1.0.1-25-gfd47d00"},
		{"1.2.3-rc1", Version{Major: 1, Minor: 2, Patch: 3, Prerelease: "rc1"}, "1.2.3-rc1"},
		// Build metadata is dropped: semver says it never affects precedence,
		// so keeping it would make two identical versions compare unequal.
		{"1.2.3+build7", Version{Major: 1, Minor: 2, Patch: 3}, "1.2.3"},
		{"1.2.3-rc1+build7", Version{Major: 1, Minor: 2, Patch: 3, Prerelease: "rc1"}, "1.2.3-rc1"},
	} {
		got, err := ParseVersion(tc.in)
		if err != nil {
			t.Errorf("ParseVersion(%q) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if got.String() != tc.wantString {
			t.Errorf("ParseVersion(%q).String() = %q, want %q", tc.in, got.String(), tc.wantString)
		}
		if !got.IsPrerelease() && tc.want.Prerelease != "" {
			t.Errorf("ParseVersion(%q).IsPrerelease() = false", tc.in)
		}
	}
	if _, err := ParseVersion("1.2.3-"); err == nil {
		t.Error(`ParseVersion("1.2.3-") = nil error, want one for an empty suffix`)
	}
}

func TestVersion_ComparePrereleases(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		// `git describe --tags` counts commits PAST the tag, so a describe
		// build is NEWER than the release it names — the opposite of semver's
		// reading. Ordering these the semver way broke the main channel: with
		// 1.0.2 and 1.0.2-4-g47b6a7a both in the registry, Latest() elected
		// the plain release and the canary was never offered a main build,
		// while being told to move backwards onto 1.0.2 (found 2026-08-13).
		{"1.0.2-25-gfd47d00", "1.0.2", 1},
		{"1.0.2", "1.0.2-25-gfd47d00", -1},
		// The commit count must compare numerically. Lexically "9-g..." beats
		// "10-g...", which would tell a canary it was up to date for the
		// ninety commits between them.
		{"1.0.2-10-gaaaaaaa", "1.0.2-9-gbbbbbbb", 1},
		{"1.0.2-9-gbbbbbbb", "1.0.2-10-gaaaaaaa", -1},
		{"1.0.2-26-gaaaaaaa", "1.0.2-25-gbbbbbbb", 1},
		{"1.0.2-25-gaaaaaaa", "1.0.2-25-gaaaaaaa", 0},
		// The live pair that exposed the bug.
		{"1.0.2-4-g47b6a7a", "1.0.2-3-ga1717c8", 1},
		{"1.0.2-3-ga1717c8", "1.0.2", 1},
		// A real pre-release still ranks below its release, and a describe
		// build taken from that pre-release tag sits between the two.
		{"1.0.2-rc1", "1.0.2", -1},
		{"1.0.2-rc1-3-gabc1234", "1.0.2-rc1", 1},
		{"1.0.2-rc1-3-gabc1234", "1.0.2", -1},
		{"1.0.2-rc2", "1.0.2-rc1", 1},
		// A suffix that merely looks describe-ish is not one: too few hex
		// digits, and no `-g` marker at all. Both keep semver ordering.
		{"1.0.2-3-gaaa", "1.0.2", -1},
		{"1.0.2-25", "1.0.2", -1},
		// X.Y.Z still dominates everything.
		{"1.0.3-1-gaaaaaaa", "1.0.2", 1},
		{"1.0.2-99-gaaaaaaa", "1.0.3", -1},
		// The renumber guard is untouched: a major bump still wins outright,
		// which is what keeps a stale v2.x from outranking v1.x.
		{"2.0.0-1-gaaaaaaa", "1.9.9", 1},
	} {
		a, err := ParseVersion(tc.a)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.a, err)
		}
		b, err := ParseVersion(tc.b)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.b, err)
		}
		if got := a.Compare(b); got != tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// This is where the guard that used to live in the parser now lives: a
// production cluster must never be offered a head-of-main build, and the
// canary must be offered both (a cut release IS newer than the main builds
// before it, and refusing it would strand the canary behind).
func TestChannelAccepts(t *testing.T) {
	release := Version{Major: 1, Minor: 0, Patch: 2}
	mainBuild := Version{Major: 1, Minor: 0, Patch: 2, Prerelease: "25-gfd47d00"}

	if !ChannelStable.Accepts(release) {
		t.Error("stable must accept a published release")
	}
	if ChannelStable.Accepts(mainBuild) {
		t.Error("stable must NOT accept a head-of-main build — that is the whole guard")
	}
	if !ChannelMain.Accepts(mainBuild) {
		t.Error("main must accept a head-of-main build")
	}
	if !ChannelMain.Accepts(release) {
		t.Error("main must accept a published release, or the canary strands behind one")
	}
}
