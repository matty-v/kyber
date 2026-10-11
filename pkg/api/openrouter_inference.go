package api

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const openRouterInferenceBaseURL = "https://openrouter.ai/api/v1"

var openRouterModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// Only the canonical Responses endpoint gets OpenRouter-specific rules. A
// lookalike hostname must remain an ordinary custom endpoint.
func isOpenRouterEndpoint(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "openrouter.ai") &&
		(u.Port() == "" || u.Port() == "443") &&
		strings.TrimSuffix(u.Path, "/") == "/api/v1" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func validateOpenRouterModel(model string) error {
	if !openRouterModelID.MatchString(model) || len(model) > 253 {
		return fmt.Errorf("OpenRouter Codex requires an explicit author/model ID (for example, cohere/north-mini-code:free); a real turn must verify model and account compatibility")
	}
	return nil
}
