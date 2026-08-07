package runtimedetect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultNpmRegistryURL is the npm registry endpoint for the
// @anthropic-ai/claude-code package. The poller may be pointed at a
// different host for tests.
const DefaultNpmRegistryURL = "https://registry.npmjs.org/@anthropic-ai/claude-code"

// DefaultCodexNpmRegistryURL is the npm registry endpoint for Codex CLI.
const DefaultCodexNpmRegistryURL = "https://registry.npmjs.org/@openai/codex"

// NpmClient fetches @anthropic-ai/claude-code release info from the npm
// registry. Uses stdlib net/http only — no new deps.
type NpmClient struct {
	URL        string
	HTTPClient *http.Client
}

// NewNpmClient returns an NpmClient with sane defaults. Pass an empty url
// to use DefaultNpmRegistryURL.
func NewNpmClient(url string, timeout time.Duration) *NpmClient {
	if url == "" {
		url = DefaultNpmRegistryURL
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &NpmClient{
		URL:        url,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// npmPackageResponse mirrors the subset of the npm registry response we
// care about. The full response is large; decode-then-pluck keeps memory
// reasonable and tests stable against upstream growth.
type npmPackageResponse struct {
	DistTags map[string]string         `json:"dist-tags"`
	Versions map[string]struct{}       `json:"versions"`
	Time     map[string]npmTimeWrapper `json:"time"`
}

// npmTimeWrapper accepts either an RFC3339 string (most entries) or any
// JSON value — npm emits sentinel keys like "modified" and "created"
// alongside version timestamps, and we only care about parseable times.
type npmTimeWrapper struct {
	t time.Time
}

func (w *npmTimeWrapper) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		// Skip non-string values silently — they're not version timestamps.
		w.t = time.Time{}
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		w.t = time.Time{}
		return nil
	}
	w.t = t
	return nil
}

// Fetch returns the most recent published versions, newest first, capped at
// limit entries. The dist-tags "latest" is always included as the first
// entry when present; the remainder are sorted by published time descending.
// Returns an error on network/HTTP failures or JSON malformations — the
// caller (poller) treats any error as a soft failure and serves last-good.
func (c *NpmClient) Fetch(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = DefaultVersionLimit
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("building npm request: %w", err)
	}
	// The npm registry honors Accept for trimmed responses, but the
	// abbreviated form omits the `time` map we need. Stick with the full
	// JSON.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kyber-runtimedetect/1 (+https://github.com/matty-v/kyber)")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling npm registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Drain a small prefix for the error message but never log secrets
		// — npm responses are public, so this is safe.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("npm registry %s: status=%d body=%q", c.URL, resp.StatusCode, body)
	}
	var pkg npmPackageResponse
	if err := json.NewDecoder(resp.Body).Decode(&pkg); err != nil {
		return nil, fmt.Errorf("decoding npm response: %w", err)
	}
	return pkg.recentVersions(limit), nil
}

// recentVersions extracts the most-recent publishable versions from the
// decoded npm response. Algorithm:
//  1. Take every key from `versions` that also has a parseable `time` entry.
//  2. Sort descending by publish time.
//  3. Cap at limit entries.
//  4. Ensure dist-tags.latest is the first element (move it if necessary).
//
// Pre-release versions (e.g. "1.0.0-beta") are filtered out so the picker
// only shows production releases. Operators who need to pin a pre-release
// can still type it manually via the existing `spec.runtimeVersion` field
// (landing in PR-C).
func (r *npmPackageResponse) recentVersions(limit int) []string {
	type entry struct {
		ver string
		at  time.Time
	}
	var entries []entry
	for v := range r.Versions {
		if !isStableSemver(v) {
			continue
		}
		t, ok := r.Time[v]
		if !ok || t.t.IsZero() {
			continue
		}
		entries = append(entries, entry{ver: v, at: t.t})
	}
	sort.Slice(entries, func(i, j int) bool {
		// Newer first; break ties by string compare for determinism.
		if !entries[i].at.Equal(entries[j].at) {
			return entries[i].at.After(entries[j].at)
		}
		return entries[i].ver > entries[j].ver
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ver)
	}
	latest := r.DistTags["latest"]
	if latest == "" {
		return out
	}
	// Promote latest to position 0 if present in the truncated list. If
	// latest isn't in the truncated list (very old release), prepend it
	// and drop the last entry to keep the cap.
	for i, v := range out {
		if v == latest {
			if i == 0 {
				return out
			}
			out = append([]string{latest}, append(out[:i], out[i+1:]...)...)
			return out
		}
	}
	if len(out) >= limit {
		out = out[:limit-1]
	}
	return append([]string{latest}, out...)
}

// isStableSemver reports whether v matches `MAJOR.MINOR.PATCH` with all
// numeric components and no pre-release / build-metadata suffix. We don't
// need full SemVer 2.0 here — the design only needs to filter pre-releases
// out of the picker. Anything fancier than digits.digits.digits is rejected.
func isStableSemver(v string) bool {
	if strings.ContainsAny(v, "-+ \t") {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}
