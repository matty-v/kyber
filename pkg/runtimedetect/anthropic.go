package runtimedetect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultAnthropicModelsURL is the Anthropic Models API endpoint. Tests
// point this at an httptest server.
const DefaultAnthropicModelsURL = "https://api.anthropic.com/v1/models"

// DefaultAnthropicAPIVersion is the value sent in the `anthropic-version`
// header. Anthropic's API requires this header on every request.
const DefaultAnthropicAPIVersion = "2023-06-01"

// ErrAnthropicKeyMissing is returned when the poller is asked to call the
// Anthropic Models API but no operator-supplied key is configured. The
// poller treats this as a soft failure — /available still serves the last
// known list (which may be empty).
var ErrAnthropicKeyMissing = errors.New("runtimedetect: anthropic api key not configured")

// AnthropicClient fetches the list of available models from the Anthropic
// Models API. Stdlib net/http only.
type AnthropicClient struct {
	URL        string
	APIVersion string
	HTTPClient *http.Client
}

// NewAnthropicClient returns a client with sane defaults.
func NewAnthropicClient(url, apiVersion string, timeout time.Duration) *AnthropicClient {
	if url == "" {
		url = DefaultAnthropicModelsURL
	}
	if apiVersion == "" {
		apiVersion = DefaultAnthropicAPIVersion
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &AnthropicClient{
		URL:        url,
		APIVersion: apiVersion,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// anthropicModelsResponse mirrors the Anthropic Models API response.
// Only the fields we surface are decoded.
type anthropicModelsResponse struct {
	Data    []anthropicModelEntry `json:"data"`
	HasMore bool                  `json:"has_more"`
}

type anthropicModelEntry struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
	// MaxInputTokens is the model's context-window size, returned per model
	// object on the /v1/models list response (kyber#488). When present
	// (> 0) the poller adopts it as the real, known window; when absent the
	// field stays zero and we fall back to DefaultContextWindowFloor.
	MaxInputTokens int64 `json:"max_input_tokens"`
}

// Fetch returns the models list, in the order the upstream returned them.
// apiKey is the operator-supplied bearer token; empty key returns
// ErrAnthropicKeyMissing without making a network call.
//
// Errors are non-fatal at the caller (poller) — the cache continues to
// serve the last known list.
func (c *AnthropicClient) Fetch(ctx context.Context, apiKey string) ([]Model, error) {
	if apiKey == "" {
		return nil, ErrAnthropicKeyMissing
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("building anthropic request: %w", err)
	}
	// The Models API uses x-api-key, not Authorization: Bearer.
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", c.APIVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kyber-runtimedetect/1 (+https://github.com/matty-v/kyber)")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling anthropic models api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Bound the body read so a hostile/buggy upstream can't blow up
		// memory. Don't log the body — Anthropic error payloads may echo
		// request context.
		_, _ = io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("anthropic models api: status=%d", resp.StatusCode)
	}
	var body anthropicModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding anthropic response: %w", err)
	}
	out := make([]Model, 0, len(body.Data))
	for _, m := range body.Data {
		// Filter to model-type entries only; the API may surface other
		// object types in the future.
		if m.Type != "" && m.Type != "model" {
			continue
		}
		if m.ID == "" {
			continue
		}
		// Adopt the API-reported context window when present. The Models API
		// returns max_input_tokens per model (kyber#488), so a newly-published
		// model auto-gets its real window with no kyber-model-context-windows
		// edit. A missing/non-positive value (<= 0) is treated as absent and
		// falls back to the safe floor flagged as unknown.
		contextWindow := DefaultContextWindowFloor
		known := false
		if m.MaxInputTokens > 0 {
			contextWindow = m.MaxInputTokens
			known = true
		}
		out = append(out, Model{
			ID:                 m.ID,
			DisplayName:        m.DisplayName,
			ContextWindow:      contextWindow,
			ContextWindowKnown: known,
		})
	}
	return out, nil
}
