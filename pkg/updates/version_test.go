package updates

import "testing"

func TestParseVersion_AcceptsStrictSemver(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Version
	}{
		{"1.2.3", Version{1, 2, 3}},
		{"v1.2.3", Version{1, 2, 3}},
		{" v0.0.1 ", Version{0, 0, 1}},
		{"10.20.30", Version{10, 20, 30}},
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

// Strictness is the safety property, not a style choice. Each of these is a
// string that a permissive parser would coerce into a plausible-looking
// version, which is how an update checker ends up offering a build label or a
// git-describe string as if it were a release.
func TestParseVersion_RejectsAnythingElse(t *testing.T) {
	for _, in := range []string{
		"",
		"v",
		"1.2",
		"1.2.3.4",
		"1.2.x",
		"1.2.3-rc1",        // pre-release: not something we upgrade to
		"1.2.3+build7",     // build metadata
		"2.1.1-7-gd64fbbd", // git describe — the exact shape that broke the old canary label
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
