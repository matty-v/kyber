// Package githubapp mints and revokes installation access tokens for the
// Kyber GitHub App. The App's private key and App/Installation IDs live in
// the kyber-github-app Secret in kyber-system; this package is the single
// blessed code path that turns those credentials into a short-lived token
// scoped to an agent's identity repo operations.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is api.github.com. Tests override via WithBaseURL.
const DefaultBaseURL = "https://api.github.com"

// jwtTTL is how long the App JWT is valid after `now`. GitHub's hard
// ceiling is 10 minutes on the full signed window (iat → exp); with a 60s
// backdate we burn 8m + 60s = 9m of that window and keep a real ~1 minute
// cushion against forward clock skew.
const jwtTTL = 8 * time.Minute

// jwtBackdate compensates for clock skew between this process and GitHub's
// clock. Matches GitHub's published sample code.
const jwtBackdate = 60 * time.Second

// maxResponseBytes caps GitHub response bodies before the process reads
// them into memory. The real responses are sub-kilobyte; this guards
// against a misbehaving proxy returning a huge body.
const maxResponseBytes = 1 << 20 // 1 MiB

// Config carries everything needed to talk to the GitHub App API.
type Config struct {
	// AppID is the App's numeric ID (shown on the App settings page).
	AppID int64
	// InstallationID is the ID from /settings/installations/<n> after
	// installing the App on an account.
	InstallationID int64
	// PrivateKey is the App's private key, parsed from the .pem GitHub
	// issued at registration time.
	PrivateKey *rsa.PrivateKey
}

// Validate fails loudly if any field is zero. Call this after loading a
// Config from a Secret so misconfigurations surface at startup, not on the
// first token mint.
func (c Config) Validate() error {
	switch {
	case c.AppID == 0:
		return errors.New("githubapp: AppID is zero")
	case c.InstallationID == 0:
		return errors.New("githubapp: InstallationID is zero")
	case c.PrivateKey == nil:
		return errors.New("githubapp: PrivateKey is nil")
	}
	return nil
}

// Client mints installation tokens. It is safe for concurrent use.
type Client struct {
	cfg     Config
	baseURL string
	http    *http.Client
	now     func() time.Time
}

// Option customises a Client. Used by tests (httptest server, fixed clock)
// and by callers who need a custom HTTP client.
type Option func(*Client)

// WithBaseURL overrides the GitHub API base URL. Tests point this at an
// httptest server.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient overrides the HTTP client (timeouts, transport).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithClock overrides the time source. Tests inject a fixed clock so JWT
// iat/exp are deterministic.
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// NewClient returns a Client ready to mint tokens.
func NewClient(cfg Config, opts ...Option) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &Client{
		cfg:     cfg,
		baseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: 15 * time.Second},
		now:     time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// InstallationToken is the subset of GitHub's access_tokens response Kyber
// cares about. Other fields (repositories, single_file_paths) are ignored.
type InstallationToken struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions,omitempty"`
	RepositorySelection string            `json:"repository_selection,omitempty"`
}

// scopedTokenRequest is the optional POST body for access_tokens. A nil body
// (passed to mintToken) requests an installation-wide token; a non-nil body
// narrows the token to the named repositories and/or permission set. All
// fields are omitempty so a zero-value struct marshals to "{}".
type scopedTokenRequest struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// MintInstallationToken calls POST /app/installations/{id}/access_tokens.
// The returned token has the permissions the App was registered with;
// callers should not assume more.
func (c *Client) MintInstallationToken(ctx context.Context) (*InstallationToken, error) {
	return c.mintToken(ctx, nil)
}

// MintScopedToken mints an installation token narrowed to the named
// repositories (bare repo names, e.g. "dave-agent" — NOT "owner/repo") and
// permission set (e.g. {"contents":"write"}). Both narrow the token below the
// installation's full grant; GitHub rejects (422) a scope the installation
// does not hold. Requesting neither repositories nor permissions is rejected
// so a caller cannot accidentally obtain an installation-wide token while
// believing it is scoped.
func (c *Client) MintScopedToken(ctx context.Context, repositories []string, permissions map[string]string) (*InstallationToken, error) {
	if len(repositories) == 0 && len(permissions) == 0 {
		return nil, errors.New("githubapp: MintScopedToken requires repositories and/or permissions")
	}
	return c.mintToken(ctx, &scopedTokenRequest{Repositories: repositories, Permissions: permissions})
}

// mintToken is the shared implementation for MintInstallationToken and
// MintScopedToken. A nil reqBody preserves the original empty-POST behaviour
// (http.NoBody + explicit Content-Length: 0) so middleboxes that reject empty
// POSTs without a Content-Length header (seen with some corp proxies) don't
// 411 us; a non-nil reqBody is sent as JSON.
func (c *Client) mintToken(ctx context.Context, reqBody *scopedTokenRequest) (*InstallationToken, error) {
	jwtStr, err := c.mintJWT()
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.baseURL, c.cfg.InstallationID)

	var body io.Reader = http.NoBody
	var contentLen int64
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("githubapp: marshaling scoped token request: %w", err)
		}
		body = bytes.NewReader(b)
		contentLen = int64(len(b))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("githubapp: building access_tokens request: %w", err)
	}
	req.ContentLength = contentLen
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubapp: POST access_tokens: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("githubapp: reading access_tokens response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("githubapp: access_tokens: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}

	var tok InstallationToken
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return nil, fmt.Errorf("githubapp: decoding access_tokens response: %w", err)
	}
	if tok.Token == "" {
		return nil, errors.New("githubapp: access_tokens response missing token field")
	}
	return &tok, nil
}

// RevokeInstallationToken calls DELETE /installation/token with the given
// token as the bearer. Idempotent from the caller's perspective — a
// not-found response is treated as success.
func (c *Client) RevokeInstallationToken(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("githubapp: empty token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/installation/token", nil)
	if err != nil {
		return fmt.Errorf("githubapp: building installation/token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("githubapp: DELETE installation/token: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if readErr != nil {
			return fmt.Errorf("githubapp: installation/token: HTTP %d: reading body: %w", resp.StatusCode, readErr)
		}
		return fmt.Errorf("githubapp: installation/token: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
}

// Repository describes a single GitHub repo accessible to the App
// installation. Mirrors the subset of fields Kyber cares about; the GitHub
// payload has many more (owner block, urls, dates, etc.) which we drop.
type Repository struct {
	FullName      string `json:"fullName"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"defaultBranch"`
	IsTemplate    bool   `json:"isTemplate"`
}

// installationRepositoriesPage is the on-the-wire shape returned by
// /installation/repositories. We rename fields into the Repository struct
// above on read so the public surface stays Kyber-flavoured.
type installationRepositoriesPage struct {
	TotalCount   int `json:"total_count"`
	Repositories []struct {
		FullName      string `json:"full_name"`
		Description   string `json:"description"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
		IsTemplate    bool   `json:"is_template"`
	} `json:"repositories"`
}

// installationRepositoriesPerPage is GitHub's max — pagination still works
// at lower values but pulls more requests. 100 minimizes round-trips.
const installationRepositoriesPerPage = 100

// listRepositoriesMaxPages caps how deep we paginate before bailing. The
// Kyber GitHub App is typically installed on single-digit repos; this guard
// is for sanity, not real-world traffic.
const listRepositoriesMaxPages = 50

// ListInstallationRepositories returns every repository the App
// installation has access to, paginated under the hood. Each call mints a
// fresh installation token, makes the GET, and revokes the token at the
// end. Callers should cache the result if they need rate-limit headroom —
// see #134 for the API-layer 60s cache.
func (c *Client) ListInstallationRepositories(ctx context.Context) ([]Repository, error) {
	tok, err := c.MintInstallationToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("githubapp: list repos: minting token: %w", err)
	}
	defer func() {
		// Best-effort revoke. A failed revoke leaves a 1h-TTL token alive;
		// not a security incident at this surface area.
		_ = c.RevokeInstallationToken(ctx, tok.Token)
	}()

	out := make([]Repository, 0, installationRepositoriesPerPage)
	for page := 1; page <= listRepositoriesMaxPages; page++ {
		url := fmt.Sprintf("%s/installation/repositories?per_page=%d&page=%d",
			c.baseURL, installationRepositoriesPerPage, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("githubapp: building installation/repositories request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("githubapp: GET installation/repositories: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("githubapp: reading installation/repositories: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("githubapp: installation/repositories: HTTP %d: %s",
				resp.StatusCode, bytes.TrimSpace(body))
		}

		var pageBody installationRepositoriesPage
		if err := json.Unmarshal(body, &pageBody); err != nil {
			return nil, fmt.Errorf("githubapp: decoding installation/repositories: %w", err)
		}
		for _, r := range pageBody.Repositories {
			out = append(out, Repository{
				FullName:      r.FullName,
				Description:   r.Description,
				Private:       r.Private,
				DefaultBranch: r.DefaultBranch,
				IsTemplate:    r.IsTemplate,
			})
		}
		// Done when the current page returned fewer than the max — GitHub's
		// pagination is exhausted.
		if len(pageBody.Repositories) < installationRepositoriesPerPage {
			return out, nil
		}
	}
	// Hit the page cap; return what we have rather than erroring. The cap
	// is a safety guard; legitimate operators never reach it.
	return out, nil
}

// RepoExists checks whether a single GitHub repo exists. Returns true on
// 200 OK, false on 404 Not Found. Other statuses surface as errors. Used
// by the create-new-from-template collision check.
func (c *Client) RepoExists(ctx context.Context, owner, repo string) (bool, error) {
	if owner == "" || repo == "" {
		return false, errors.New("githubapp: owner and repo required")
	}
	tok, err := c.MintInstallationToken(ctx)
	if err != nil {
		return false, fmt.Errorf("githubapp: repo exists: minting token: %w", err)
	}
	defer func() {
		_ = c.RevokeInstallationToken(ctx, tok.Token)
	}()

	url := fmt.Sprintf("%s/repos/%s/%s", c.baseURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("githubapp: building repos request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("githubapp: GET repos: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return false, fmt.Errorf("githubapp: repos: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
}

// mintJWT builds the RS256-signed JWT GitHub accepts as App authentication.
// Tests assert signature validity + claims by intercepting the bearer token
// in the Authorization header, which is why this stays unexported.
func (c *Client) mintJWT() (string, error) {
	now := c.now()
	headerBytes, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("githubapp: marshaling JWT header: %w", err)
	}
	payloadBytes, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtTTL).Unix(),
		"iss": strconv.FormatInt(c.cfg.AppID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("githubapp: marshaling JWT payload: %w", err)
	}
	signingInput := base64url(headerBytes) + "." + base64url(payloadBytes)

	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.cfg.PrivateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: signing JWT: %w", err)
	}
	return signingInput + "." + base64url(sig), nil
}

func base64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// ParsePrivateKey parses a PEM-encoded RSA private key. Accepts both PKCS1
// ("RSA PRIVATE KEY") and PKCS8 ("PRIVATE KEY") since GitHub has issued both
// formats at different points.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("githubapp: no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("githubapp: PKCS8 key is not RSA")
		}
		return rk, nil
	default:
		return nil, fmt.Errorf("githubapp: unsupported PEM block type %q", block.Type)
	}
}
