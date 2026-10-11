package usagestore

import (
	"errors"
	"testing"
)

func TestRouteRevisionRejectsCredentialsAndLookalikeHosts(t *testing.T) {
	for _, tc := range []struct {
		route RouteRevision
		valid bool
	}{
		{RouteRevision{Runtime: "claude", Kind: "native"}, true},
		{RouteRevision{Runtime: "claude", Kind: "direct-api"}, true},
		{RouteRevision{Runtime: "hermes", Kind: "openrouter", EndpointURL: "https://openrouter.ai/api/v1"}, true},
		{RouteRevision{Runtime: "codex", Kind: "custom", EndpointURL: "https://llm.example.test/v1"}, true},
		{RouteRevision{Runtime: "codex", Kind: "custom", EndpointURL: "https://key@llm.example.test/v1"}, false},
		{RouteRevision{Runtime: "codex", Kind: "custom", EndpointURL: "https://llm.example.test/v1?key=secret"}, false},
		{RouteRevision{Runtime: "codex", Kind: "openrouter", EndpointURL: "https://openrouter.ai.evil.test/api/v1"}, false},
		{RouteRevision{Runtime: "claude", Kind: "native", EndpointURL: "https://llm.example.test"}, false},
	} {
		err := tc.route.Validate()
		if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrInvalid) {
			t.Errorf("route=%+v valid=%v err=%v", tc.route, tc.valid, err)
		}
	}
}
