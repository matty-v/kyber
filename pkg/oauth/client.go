// Package oauth talks to Anthropic's OAuth token endpoint on behalf of Kyber.
// Only two operations are supported: authorization_code exchange (during agent
// create) and refresh_token (during agent pod boot).
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ClaudeCodeClientID is Claude Code CLI's public OAuth client_id. Kyber reuses
// it because we have no partner relationship; agents authenticate as if they
// were the CLI itself.
const ClaudeCodeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// ManualRedirectURI is Anthropic's hosted "manual" callback page, registered
// with the Claude Code CLI OAuth client. After authorization Anthropic
// redirects to this URL and displays the authorization code in a clean UI,
// so the user can copy it without inspecting a broken localhost address bar.
const ManualRedirectURI = "https://platform.claude.com/oauth/code/callback"

// DefaultTokenURL is Anthropic's production token endpoint. Tests override via
// NewClient with the mock server URL.
const DefaultTokenURL = "https://platform.claude.com/v1/oauth/token"

// Tokens is Anthropic's token response shape.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// Client talks to the token endpoint.
type Client struct {
	tokenURL string
	http     *http.Client
}

// NewClient returns a Client that talks to the given token endpoint. Pass
// DefaultTokenURL in prod; in tests pass the mockserver URL.
func NewClient(tokenURL string) *Client {
	return &Client{
		tokenURL: tokenURL,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

// ExchangeAuthorizationCode swaps a PKCE code + verifier for tokens. `state`
// must match the state used in the authorize URL — Anthropic requires it in
// the exchange body too.
func (c *Client) ExchangeAuthorizationCode(ctx context.Context, code, verifier, state string) (*Tokens, error) {
	return c.post(ctx, map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     ClaudeCodeClientID,
		"redirect_uri":  ManualRedirectURI,
		"code_verifier": verifier,
		"state":         state,
	})
}

// RefreshAccessToken exchanges a refresh token for a fresh access token. The
// returned Tokens may carry a rotated refresh_token — callers must persist it.
func (c *Client) RefreshAccessToken(ctx context.Context, refreshToken string) (*Tokens, error) {
	return c.post(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     ClaudeCodeClientID,
		"refresh_token": refreshToken,
	})
}

type tokenErr struct {
	Code string
	Desc string
}

func (e *tokenErr) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Desc) }

// IsInvalidGrant reports whether err is a 4xx invalid_grant response. Handlers
// use this to distinguish user-fixable errors (re-authorize) from transport
// failures (retry).
func IsInvalidGrant(err error) bool {
	var te *tokenErr
	return errors.As(err, &te) && te.Code == "invalid_grant"
}

func (c *Client) post(ctx context.Context, body map[string]string) (*Tokens, error) {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Anthropic's platform.claude.com is fronted by Cloudflare Bot Management.
	// Generic User-Agents (e.g. Go's default "Go-http-client/1.1") trigger 429
	// rate_limit_error responses even with valid credentials. Use a recognisable
	// Kyber-specific UA so Cloudflare lets the request through.
	req.Header.Set("User-Agent", "kyber-oauth/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB ceiling
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &errResp)
		if errResp.Error == "" {
			errResp.Error = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return nil, &tokenErr{Code: errResp.Error, Desc: errResp.ErrorDescription}
	}
	var out Tokens
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return nil, fmt.Errorf("token endpoint returned incomplete response: %s", string(raw))
	}
	return &out, nil
}
