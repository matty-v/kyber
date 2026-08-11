package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultRepo is the upstream Kyber repository. Overridable so a fork can
// track its own releases rather than ours.
const DefaultRepo = "matty-v/kyber"

// DefaultFeedBaseURL is GitHub's REST API. Overridable for tests and for
// GitHub Enterprise installs.
const DefaultFeedBaseURL = "https://api.github.com"

// Release is one published release, reduced to what an update decision needs.
type Release struct {
	Version     Version
	Tag         string
	PublishedAt time.Time
	URL         string
}

// FeedClient reads published releases.
//
// It asks GitHub for /releases/latest rather than listing tags and ranking
// them. That is deliberate: /releases/latest returns the release GitHub itself
// marks as latest (release.yml sets `make_latest: 'true'`), so the answer comes
// from an explicit publishing decision rather than from our idea of version
// ordering. Ranking tags ourselves is how you end up offering a version from a
// previous numbering era — Kyber still has v2.7.x images in GHCR from before
// the OSS renumber, and any naive "highest semver wins" ranks those above
// v1.0.1.
type FeedClient struct {
	// HTTPClient is used for the request. Nil uses a client with a sane
	// timeout — never http.DefaultClient, which has none.
	HTTPClient *http.Client
	// Repo is "owner/name". Empty uses DefaultRepo.
	Repo string
	// BaseURL is the API root. Empty uses DefaultFeedBaseURL.
	BaseURL string
	// Token is an optional GitHub token. Unauthenticated works fine for a
	// public repo at an hourly cadence (60 requests/hour/IP); a token raises
	// that ceiling and is required if the repo is private.
	Token string
}

func (c *FeedClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *FeedClient) repo() string {
	if c.Repo != "" {
		return c.Repo
	}
	return DefaultRepo
}

func (c *FeedClient) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return DefaultFeedBaseURL
}

// Latest returns the newest published release.
//
// A repository with no releases yet is not an error — it returns (nil, nil).
// A fresh fork legitimately has none, and an operator should see "nothing
// published yet" rather than a red error they cannot act on.
func (c *FeedClient) Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.baseURL(), c.repo())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("release feed request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// Deliberately an ERROR, not a quiet "no update".
		//
		// GitHub returns 404 for all of: a repo with no releases yet, a typo'd
		// `updates.repo`, and a private repo read without a token. They are
		// indistinguishable over the wire. Returning (nil, nil) made the
		// checker stamp a fresh LastChecked with no error, so a cluster
		// pointed at nothing rendered "checked just now, up to date" forever —
		// silence reading as success, which is the failure mode this whole
		// feature exists to prevent.
		//
		// The cost is that a genuinely fresh fork with no releases shows a
		// message instead of nothing. That is the right trade: it is benign,
		// it is accurate, and it names its own fix.
		return nil, fmt.Errorf("no published release found for %s — the repository has no releases yet, or `updates.repo` is wrong, or it is private and needs a token", c.repo())
	case http.StatusForbidden, http.StatusTooManyRequests:
		return nil, fmt.Errorf("release feed rate-limited (HTTP %d) for %s; set a GitHub token to raise the limit", resp.StatusCode, c.repo())
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("release feed returned HTTP %d for %s: %s", resp.StatusCode, c.repo(), strings.TrimSpace(string(body)))
	}

	var payload struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Draft       bool   `json:"draft"`
		Prerelease  bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("release feed response: %w", err)
	}
	// /releases/latest already excludes drafts and prereleases. Checked anyway
	// so a future switch to /releases (which does not) cannot quietly start
	// offering prereleases to production clusters.
	if payload.Draft || payload.Prerelease {
		return nil, nil
	}

	v, err := ParseVersion(payload.TagName)
	if err != nil {
		// A tag we cannot parse is a tag we will not offer. Surfacing the
		// error beats silently reporting "up to date" against a release we
		// failed to understand.
		return nil, fmt.Errorf("latest release tag %q: %w", payload.TagName, err)
	}

	rel := &Release{Version: v, Tag: payload.TagName, URL: payload.HTMLURL}
	if payload.PublishedAt != "" {
		if ts, err := time.Parse(time.RFC3339, payload.PublishedAt); err == nil {
			rel.PublishedAt = ts
		}
	}
	return rel, nil
}
