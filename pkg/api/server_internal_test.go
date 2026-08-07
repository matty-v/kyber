package api

import (
	"testing"

	"github.com/matty-v/kyber/pkg/oauth"
)

func TestServer_AnthropicTokenURL_OverrideUsed(t *testing.T) {
	s := &Server{AnthropicTokenURL: "http://mock:9999/v1/oauth/token"}
	if got := s.anthropicTokenURL(); got != "http://mock:9999/v1/oauth/token" {
		t.Fatalf("want override, got %q", got)
	}
}

func TestServer_AnthropicTokenURL_DefaultFallback(t *testing.T) {
	s := &Server{}
	if got := s.anthropicTokenURL(); got != oauth.DefaultTokenURL {
		t.Fatalf("want default, got %q", got)
	}
}
