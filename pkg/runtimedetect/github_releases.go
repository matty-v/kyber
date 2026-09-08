package runtimedetect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"time"
)

const DefaultHermesReleasesURL = "https://api.github.com/repos/NousResearch/hermes-agent/releases"

var stableVersionInReleaseName = regexp.MustCompile(`(?:^|\s)v([0-9]+\.[0-9]+\.[0-9]+)(?:\s|$|\()`)

// GitHubReleasesClient fetches public release metadata for source-distributed
// runtimes such as Hermes. It requires no credential and uses stdlib HTTP.
type GitHubReleasesClient struct {
	URL        string
	HTTPClient *http.Client
}

func NewGitHubReleasesClient(url string, timeout time.Duration) *GitHubReleasesClient {
	if url == "" {
		url = DefaultHermesReleasesURL
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &GitHubReleasesClient{URL: url, HTTPClient: &http.Client{Timeout: timeout}}
}

type githubRelease struct {
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

func (c *GitHubReleasesClient) Fetch(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = DefaultVersionLimit
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("building GitHub releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kyber-runtimedetect/1 (+https://github.com/matty-v/kyber)")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling GitHub releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub releases %s: status=%d body=%q", c.URL, resp.StatusCode, body)
	}
	var releases []githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding GitHub releases: %w", err)
	}
	sort.SliceStable(releases, func(i, j int) bool { return releases[i].PublishedAt.After(releases[j].PublishedAt) })
	seen := make(map[string]struct{}, len(releases))
	versions := make([]string, 0, min(limit, len(releases)))
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		match := stableVersionInReleaseName.FindStringSubmatch(release.Name)
		if match == nil {
			continue
		}
		if _, ok := seen[match[1]]; ok {
			continue
		}
		seen[match[1]] = struct{}{}
		versions = append(versions, match[1])
		if len(versions) == limit {
			break
		}
	}
	return versions, nil
}
