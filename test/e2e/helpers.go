//go:build e2e

// Package e2e contains the end-to-end test suite for the Kyber platform.
// Tests in this package require a running k3d cluster with Kyber installed.
// Run via: go test -tags e2e -v -timeout 25m ./test/e2e/...
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

const (
	// defaultAPIKey is the API key set in values-test.yaml.
	defaultAPIKey = "test-api-key-e2e"

	// defaultWebhookSecret is the webhook secret set in values-test.yaml.
	defaultWebhookSecret = "test-webhook-secret-e2e"

	// defaultBaseURL is used when KYBER_E2E_BASE_URL is not set.
	// The test setup port-forwards :8080 to localhost:18080.
	defaultBaseURL = "http://localhost:18080"

	// pollInterval is how often WaitForCondition polls.
	pollInterval = 2 * time.Second
)

// APIClient wraps an http.Client with Kyber API conventions.
type APIClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// newAPIClient returns an APIClient configured from environment variables or defaults.
// KYBER_E2E_BASE_URL overrides the base URL (useful when port-forwarding to a different port).
// KYBER_E2E_API_KEY overrides the API key.
func newAPIClient(t *testing.T) *APIClient {
	t.Helper()
	base := os.Getenv("KYBER_E2E_BASE_URL")
	if base == "" {
		base = defaultBaseURL
	}
	key := os.Getenv("KYBER_E2E_API_KEY")
	if key == "" {
		key = defaultAPIKey
	}
	return &APIClient{
		BaseURL: base,
		APIKey:  key,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Get performs an authenticated GET to path and decodes the JSON response into out.
// Pass nil for out to discard the body.
func (c *APIClient) Get(path string, out interface{}) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if out != nil {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp, err
		}
		if err := json.Unmarshal(body, out); err != nil {
			return resp, fmt.Errorf("decode %s: %w (body: %s)", path, err, body)
		}
	}
	return resp, nil
}

// GetRaw performs an authenticated GET and returns the status code and raw body.
func (c *APIClient) GetRaw(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// GetNoAuth performs a GET without an Authorization header.
func (c *APIClient) GetNoAuth(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}

// Post performs an authenticated POST to path with the JSON-encoded body in,
// and decodes the JSON response into out. Pass nil for out to discard the body.
func (c *APIClient) Post(path string, in interface{}, out interface{}) (*http.Response, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if out != nil {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp, err
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return resp, fmt.Errorf("decode POST %s: %w (body: %s)", path, err, respBody)
		}
	}
	return resp, nil
}

// PostRaw performs an authenticated POST and returns the raw status + body.
func (c *APIClient) PostRaw(path string, in interface{}) (int, []byte, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, nil
}

// Delete performs an authenticated DELETE to path.
func (c *APIClient) Delete(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}

// PostWebhook posts a Telegram-style webhook payload to the given path without an API key,
// but with the webhook secret header Kyber uses for webhook auth validation.
func (c *APIClient) PostWebhook(path string, in interface{}, secret string) (int, []byte, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, nil
}

// WaitForCondition polls fn every pollInterval until it returns true or the timeout expires.
// fn should return (done bool, err error). A non-nil error causes an immediate return.
func WaitForCondition(ctx context.Context, timeout time.Duration, description string, fn func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := fn()
		if err != nil {
			return fmt.Errorf("%s: %w", description, err)
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: timed out after %s", description, timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: context cancelled: %w", description, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// WaitForMachinePhase polls GET /api/v1/machines/{name} until phase matches or timeout.
func WaitForMachinePhase(ctx context.Context, c *APIClient, name, phase string, timeout time.Duration) error {
	return WaitForCondition(ctx, timeout, fmt.Sprintf("machine %s phase=%s", name, phase), func() (bool, error) {
		var resp machineResponse
		httpResp, err := c.Get("/api/v1/machines/"+name, &resp)
		if err != nil {
			return false, err
		}
		if httpResp.StatusCode == http.StatusNotFound {
			if phase == "deleted" {
				return true, nil
			}
			return false, nil
		}
		if httpResp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("unexpected status %d", httpResp.StatusCode)
		}
		return resp.Phase == phase, nil
	})
}

// WaitForMachineDeleted polls until the machine returns 404.
func WaitForMachineDeleted(ctx context.Context, c *APIClient, name string, timeout time.Duration) error {
	return WaitForCondition(ctx, timeout, fmt.Sprintf("machine %s deleted", name), func() (bool, error) {
		req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/api/v1/machines/"+name, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return false, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusNotFound, nil
	})
}

// WaitForAgentPhase polls GET /api/v1/agents/{name} until phase matches or timeout.
func WaitForAgentPhase(ctx context.Context, c *APIClient, name, phase string, timeout time.Duration) error {
	return WaitForCondition(ctx, timeout, fmt.Sprintf("agent %s phase=%s", name, phase), func() (bool, error) {
		var resp agentResponse
		httpResp, err := c.Get("/api/v1/agents/"+name, &resp)
		if err != nil {
			return false, err
		}
		if httpResp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		if httpResp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("unexpected status %d", httpResp.StatusCode)
		}
		return resp.Phase == phase, nil
	})
}

// WaitForAgentDeleted polls until the agent returns 404.
func WaitForAgentDeleted(ctx context.Context, c *APIClient, name string, timeout time.Duration) error {
	return WaitForCondition(ctx, timeout, fmt.Sprintf("agent %s deleted", name), func() (bool, error) {
		req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/api/v1/agents/"+name, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return false, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusNotFound, nil
	})
}

// machineResponse is a minimal parse of the machine GET response for phase polling.
type machineResponse struct {
	ID    string `json:"id"`
	Phase string `json:"phase"`
}

// agentResponse is a minimal parse of the agent GET response for phase polling.
type agentResponse struct {
	ID    string `json:"id"`
	Phase string `json:"phase"`
}

// machineListResponse is a minimal parse of GET /api/v1/machines.
type machineListResponse struct {
	Items []machineResponse `json:"items"`
}

// agentListResponse is a minimal parse of GET /api/v1/agents.
type agentListResponse struct {
	Items []agentResponse `json:"items"`
}

// requireStatus is a test helper that marks the test failed if got != want.
func requireStatus(t *testing.T, want, got int, body []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("expected HTTP %d, got %d (body: %s)", want, got, body)
	}
}

// requireNoErr marks the test failed if err is non-nil.
func requireNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
