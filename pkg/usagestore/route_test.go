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

func TestRouteRevisionForAgentConfiguration(t *testing.T) {
	for _, tc := range []struct{ runtime, auth, endpoint, want string }{
		{"claude-code", "oauth", "", "native"},
		{"claude-code", "api-key", "", "direct-api"},
		{"codex", "api-key", "", "direct-api"},
		{"hermes", "", "", "openrouter"},
		{"codex", "", "https://openrouter.ai/api/v1", "openrouter"},
		{"codex", "", "https://openrouter.ai.evil.test/api/v1", "custom"},
	} {
		got, err := RouteRevisionFor(tc.runtime, tc.auth, tc.endpoint)
		if err != nil || got.Kind != tc.want {
			t.Errorf("%s/%s/%s => %+v err=%v", tc.runtime, tc.auth, tc.endpoint, got, err)
		}
	}
	a, _ := RouteRevisionFor("codex", "", "https://llm.example.test/v1")
	b, _ := RouteRevisionFor("codex", "", "https://other.example.test/v1")
	aID, _ := a.FingerprintHex()
	bID, _ := b.FingerprintHex()
	if aID == bID || len(aID) != 64 || len(bID) != 64 {
		t.Fatalf("fingerprints=%q/%q", aID, bID)
	}
}
