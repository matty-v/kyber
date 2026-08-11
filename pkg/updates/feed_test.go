package updates

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func feedServing(t *testing.T, status int, body string) (*FeedClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/repos/matty-v/kyber/releases/latest"; got != want {
			t.Errorf("requested %q, want %q — the client must ask for the release GitHub marks latest, not list tags", got, want)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &FeedClient{BaseURL: srv.URL, HTTPClient: srv.Client()}, srv
}

func TestFeed_LatestParsesAPublishedRelease(t *testing.T) {
	c, _ := feedServing(t, http.StatusOK, `{
		"tag_name": "v1.2.3",
		"html_url": "https://github.com/matty-v/kyber/releases/tag/v1.2.3",
		"published_at": "2026-08-07T21:20:54Z",
		"draft": false,
		"prerelease": false
	}`)
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel == nil {
		t.Fatal("Latest returned nil for a published release")
	}
	if got := rel.Version.String(); got != "1.2.3" {
		t.Errorf("version = %q, want %q", got, "1.2.3")
	}
	if rel.PublishedAt.IsZero() {
		t.Error("publishedAt was not parsed")
	}
	if rel.URL == "" {
		t.Error("URL is empty — the card links this so an operator can read the notes before deciding")
	}
}

// A repo with no releases is a normal state (a fresh fork), not a failure to
// put in front of an operator who cannot act on it.
func TestFeed_LatestTreatsNoReleasesAsNoUpdate(t *testing.T) {
	c, _ := feedServing(t, http.StatusNotFound, `{"message":"Not Found"}`)
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("404 should not be an error: %v", err)
	}
	if rel != nil {
		t.Errorf("Latest = %+v, want nil", rel)
	}
}

// Rate limiting must surface as an error so the card can show "we stopped
// checking" rather than silently continuing to display a stale "up to date".
func TestFeed_LatestSurfacesRateLimiting(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		c, _ := feedServing(t, status, `{"message":"rate limit exceeded"}`)
		if _, err := c.Latest(context.Background()); err == nil {
			t.Errorf("HTTP %d did not produce an error", status)
		} else if !strings.Contains(err.Error(), "rate-limited") {
			t.Errorf("HTTP %d error should name rate limiting; got %q", status, err)
		}
	}
}

// A tag we cannot parse is a tag we will not offer, and the operator should
// hear about it — reporting "up to date" against a release we failed to
// understand is the silent-wrong-answer case.
func TestFeed_LatestErrorsOnUnparseableTag(t *testing.T) {
	c, _ := feedServing(t, http.StatusOK, `{"tag_name":"nightly","draft":false,"prerelease":false}`)
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("an unparseable tag should error, not be silently ignored")
	}
}

// /releases/latest already excludes these, but the guard exists so a future
// switch to /releases cannot quietly start offering prereleases to production.
func TestFeed_LatestIgnoresDraftsAndPrereleases(t *testing.T) {
	for _, body := range []string{
		`{"tag_name":"v9.9.9","draft":true,"prerelease":false}`,
		`{"tag_name":"v9.9.9","draft":false,"prerelease":true}`,
	} {
		c, _ := feedServing(t, http.StatusOK, body)
		rel, err := c.Latest(context.Background())
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if rel != nil {
			t.Errorf("offered %+v for %s, want nil", rel, body)
		}
	}
}

func TestFeed_LatestSurfacesUnexpectedStatus(t *testing.T) {
	c, _ := feedServing(t, http.StatusInternalServerError, `boom`)
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("HTTP 500 should error")
	}
}

// The client must not fall back to http.DefaultClient, which has no timeout —
// a hung feed would otherwise wedge the checker goroutine indefinitely.
func TestFeed_DefaultHTTPClientHasATimeout(t *testing.T) {
	c := &FeedClient{}
	if got := c.httpClient(); got.Timeout == 0 {
		t.Error("default HTTP client has no timeout; a hung release feed would wedge the poll loop")
	}
}

func TestFeed_DefaultsToUpstreamRepo(t *testing.T) {
	if got := (&FeedClient{}).repo(); got != DefaultRepo {
		t.Errorf("repo() = %q, want %q", got, DefaultRepo)
	}
	if got := (&FeedClient{Repo: "someone/fork"}).repo(); got != "someone/fork" {
		t.Errorf("repo() = %q, want the override", got)
	}
}
