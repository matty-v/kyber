package updates

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chartRegistry stands in for GHCR: a token endpoint and a tag list.
func chartRegistry(t *testing.T, tags []string, tagStatus int) *ChartFeedClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon"})
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			if r.Header.Get("Authorization") != "Bearer anon" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if tagStatus != http.StatusOK {
				w.WriteHeader(tagStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &ChartFeedClient{
		Registry:   srv.URL,
		Repository: "matty-v/charts/kyber",
		HTTPClient: srv.Client(),
	}
}

// The registry is shared with released charts, so the channel has to do the
// filtering — otherwise the canary's own builds would be offered to everyone.
func TestChartFeed_LatestPicksNewestForChannel(t *testing.T) {
	tags := []string{"1.0.1", "1.0.2-9-gaaaaaaa", "1.0.2-10-gbbbbbbb", "1.0.2"}

	c := chartRegistry(t, tags, http.StatusOK)
	main, err := c.Latest(context.Background(), ChannelMain)
	if err != nil {
		t.Fatalf("Latest(main) = %v", err)
	}
	// A describe build is commits PAST the tag it names, so it outranks the
	// bare release. Electing 1.0.2 here is what left the canary permanently
	// "up to date" and offered it a move backwards.
	if main.Version.String() != "1.0.2-10-gbbbbbbb" {
		t.Errorf("Latest(main) = %s, want 1.0.2-10-gbbbbbbb", main.Version)
	}

	stable, err := c.Latest(context.Background(), ChannelStable)
	if err != nil {
		t.Fatalf("Latest(stable) = %v", err)
	}
	if stable.Version.String() != "1.0.2" || stable.Version.IsPrerelease() {
		t.Errorf("Latest(stable) = %s, want the release 1.0.2", stable.Version)
	}
}

// TestChartFeed_MainElectsDescribeBuildOverTheReleaseItContains reproduces the
// exact registry contents that exposed the bug on the canary: v1.0.2 was cut,
// then more commits landed on main and published charts beside it. The main
// channel must elect the newest of those, not the release they contain.
func TestChartFeed_MainElectsDescribeBuildOverTheReleaseItContains(t *testing.T) {
	tags := []string{"1.0.2", "1.0.2-1-gf71b116", "1.0.2-3-ga1717c8", "1.0.2-4-g47b6a7a"}

	c := chartRegistry(t, tags, http.StatusOK)
	got, err := c.Latest(context.Background(), ChannelMain)
	if err != nil {
		t.Fatalf("Latest(main) = %v", err)
	}
	if got.Version.String() != "1.0.2-4-g47b6a7a" {
		t.Errorf("Latest(main) = %s, want 1.0.2-4-g47b6a7a", got.Version)
	}

	// And a cluster running one of those builds must not be told a plain
	// 1.0.2 is an upgrade — that is the downgrade the operator was offered.
	current, err := ParseVersion("1.0.2-3-ga1717c8")
	if err != nil {
		t.Fatalf("parse current: %v", err)
	}
	release, err := ParseVersion("1.0.2")
	if err != nil {
		t.Fatalf("parse release: %v", err)
	}
	if release.GreaterThan(current) {
		t.Errorf("1.0.2 ranked above 1.0.2-3-ga1717c8, which would offer a downgrade")
	}
}

func TestChartFeed_MainPrefersNewestPrereleaseWhenNoNewerRelease(t *testing.T) {
	c := chartRegistry(t, []string{"1.0.1", "1.0.2-9-gaaaaaaa", "1.0.2-10-gbbbbbbb"}, http.StatusOK)
	got, err := c.Latest(context.Background(), ChannelMain)
	if err != nil {
		t.Fatalf("Latest = %v", err)
	}
	// 10 beats 9 numerically; lexically it would not, and the canary would
	// sit still for ninety commits.
	if got.Version.String() != "1.0.2-10-gbbbbbbb" {
		t.Errorf("Latest(main) = %s, want 1.0.2-10-gbbbbbbb", got.Version)
	}
}

func TestChartFeed_StableIgnoresPrereleasesEntirely(t *testing.T) {
	c := chartRegistry(t, []string{"1.0.2-10-gbbb", "1.0.2-11-gccc"}, http.StatusOK)
	got, err := c.Latest(context.Background(), ChannelStable)
	if err != nil {
		t.Fatalf("Latest = %v", err)
	}
	if got != nil {
		t.Errorf("Latest(stable) = %s, want nil — none of these are releases", got.Version)
	}
}

// A 404 means nothing has ever been published, which is the honest state of a
// fresh fork. It must not read as a broken feed.
func TestChartFeed_MissingPackageIsNotAnError(t *testing.T) {
	c := chartRegistry(t, nil, http.StatusNotFound)
	got, err := c.Latest(context.Background(), ChannelMain)
	if err != nil {
		t.Fatalf("Latest = %v, want nil error for an unpublished package", err)
	}
	if got != nil {
		t.Errorf("Latest = %v, want nil", got)
	}
}

func TestChartFeed_SkipsUnparseableTags(t *testing.T) {
	c := chartRegistry(t, []string{"latest", "not-a-version", "1.0.2"}, http.StatusOK)
	got, err := c.Latest(context.Background(), ChannelMain)
	if err != nil {
		t.Fatalf("Latest = %v", err)
	}
	if got == nil || got.Version.String() != "1.0.2" {
		t.Errorf("Latest = %v, want 1.0.2 with the junk tags skipped", got)
	}
}

func TestChartFeedFromRef(t *testing.T) {
	c, err := ChartFeedFromRef("oci://ghcr.io/matty-v/charts/kyber", "")
	if err != nil {
		t.Fatalf("ChartFeedFromRef = %v", err)
	}
	if c.Registry != "ghcr.io" || c.Repository != "matty-v/charts/kyber" {
		t.Errorf("parsed = %q / %q", c.Registry, c.Repository)
	}
	if c.Ref() != "oci://ghcr.io/matty-v/charts/kyber" {
		t.Errorf("Ref() = %q", c.Ref())
	}

	// Empty is "not configured", not an error.
	if got, err := ChartFeedFromRef("  ", ""); err != nil || got != nil {
		t.Errorf("ChartFeedFromRef(empty) = %v, %v; want nil, nil", got, err)
	}
	if _, err := ChartFeedFromRef("oci://ghcr.io", ""); err == nil {
		t.Error("ChartFeedFromRef with no repository = nil error, want one")
	}
}
