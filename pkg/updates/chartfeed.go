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

// DefaultChartRegistry is the OCI registry host holding the published charts.
const DefaultChartRegistry = "ghcr.io"

// DefaultChartRepository is the repository path within that registry.
const DefaultChartRepository = "matty-v/charts/kyber"

// maxChartTags bounds how much of the tag list we will read. Every merge to
// main publishes one, so this grows steadily; we only ever want the newest.
const maxChartTags = 2000

// ChartFeedClient answers "what is the newest chart I could actually pull?" by
// listing tags in the OCI registry.
//
// The main channel reads the REGISTRY rather than the git history, which is a
// deliberate difference from the stable channel's GitHub-releases feed. A
// commit on main is not an update until its chart exists — build.yml publishes
// one per merge, but a build can fail, be skipped by path filters, or still be
// running. Deriving the version from git would let a cluster be told an update
// is available and then fail at `helm pull`, which is the same class of bug as
// publishing a chart nobody can install. Asking the registry means the answer
// is always something that exists.
type ChartFeedClient struct {
	// HTTPClient is used for both the token and tag-list requests. Nil uses a
	// client with a sane timeout.
	HTTPClient *http.Client
	// Registry is the OCI host. Empty uses DefaultChartRegistry.
	Registry string
	// Repository is the chart path. Empty uses DefaultChartRepository.
	Repository string
	// Token is an optional bearer token for a private registry. Empty fetches
	// an anonymous pull token, which is what a public package needs.
	Token string
}

func (c *ChartFeedClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (c *ChartFeedClient) registry() string {
	if c.Registry != "" {
		r := strings.TrimSuffix(c.Registry, "/")
		r = strings.TrimPrefix(r, "https://")
		return strings.TrimPrefix(r, "http://")
	}
	return DefaultChartRegistry
}

// scheme is https for any real registry. An explicit http:// prefix on
// Registry opts out, which is what a local test registry needs and what a
// plain-HTTP mirror in a closed network would need — it must be asked for, and
// never inferred.
func (c *ChartFeedClient) scheme() string {
	if strings.HasPrefix(c.Registry, "http://") {
		return "http"
	}
	return "https"
}

func (c *ChartFeedClient) repository() string {
	if c.Repository != "" {
		return strings.Trim(c.Repository, "/")
	}
	return DefaultChartRepository
}

// Ref is the pullable chart reference, without a version.
func (c *ChartFeedClient) Ref() string {
	return "oci://" + c.registry() + "/" + c.repository()
}

// Latest returns the newest chart version the channel accepts, or nil when the
// registry holds none.
//
// A tag that does not parse is skipped rather than failing the call: the
// registry is shared with whatever else may be published there, and one odd
// tag must not stop a cluster noticing a real update.
func (c *ChartFeedClient) Latest(ctx context.Context, channel Channel) (*Release, error) {
	tags, err := c.tags(ctx)
	if err != nil {
		return nil, err
	}
	var best *Release
	for _, tag := range tags {
		v, parseErr := ParseVersion(tag)
		if parseErr != nil {
			continue
		}
		if !channel.Accepts(v) {
			continue
		}
		if best == nil || v.GreaterThan(best.Version) {
			best = &Release{
				Version: v,
				Tag:     tag,
				URL:     c.scheme() + "://" + c.registry() + "/" + c.repository(),
			}
		}
	}
	return best, nil
}

// tags lists the repository's tags, fetching an anonymous pull token first
// when no explicit one is configured.
func (c *ChartFeedClient) tags(ctx context.Context) ([]string, error) {
	token := c.Token
	if token == "" {
		var err error
		token, err = c.anonymousToken(ctx)
		if err != nil {
			return nil, err
		}
	}

	url := fmt.Sprintf("%s://%s/v2/%s/tags/list?n=%d", c.scheme(), c.registry(), c.repository(), maxChartTags)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build tag-list request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("list chart tags: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// The package does not exist yet — no chart has ever been published.
		// Not an error: it is the honest state of a fresh fork, and of this
		// repo until the first release runs.
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing chart tags returned %d", resp.StatusCode)
	}

	var payload struct {
		Tags []string `json:"tags"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read tag list: %w", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse tag list: %w", err)
	}
	return payload.Tags, nil
}

// anonymousToken gets a pull-scoped token for a public repository. GHCR issues
// one to anybody who asks; the call exists because the tag-list endpoint
// refuses an unauthenticated request outright rather than serving public data.
func (c *ChartFeedClient) anonymousToken(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s://%s/token?scope=repository:%s:pull&service=%s",
		c.scheme(), c.registry(), c.repository(), c.registry())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch registry token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token request returned %d", resp.StatusCode)
	}
	var payload struct {
		Token string `json:"token"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if payload.Token == "" {
		return "", fmt.Errorf("registry returned an empty token")
	}
	return payload.Token, nil
}

// ChartFeedFromRef builds a client from a chart reference like
// "oci://ghcr.io/matty-v/charts/kyber". Returns nil for an empty ref, which
// reads as "not configured" rather than an error — an install that never sets
// it simply has no main channel.
func ChartFeedFromRef(ref, token string) (*ChartFeedClient, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(ref, "oci://"), "/")
	host, repo, ok := strings.Cut(trimmed, "/")
	if !ok || host == "" || repo == "" {
		return nil, fmt.Errorf("chart reference %q is not <registry>/<repository>", ref)
	}
	return &ChartFeedClient{Registry: host, Repository: repo, Token: token}, nil
}
