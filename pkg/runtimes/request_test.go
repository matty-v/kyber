package runtimes

import "testing"

func TestRequestMCPURLIsPodLocalStatusSidecar(t *testing.T) {
	if got, want := RequestMCPURL(), "http://127.0.0.1:8091/mcp"; got != want {
		t.Fatalf("RequestMCPURL() = %q, want %q", got, want)
	}
	if StatusSidecarForwarderPort == TelegramMCPPort || StatusSidecarForwarderPort == DiscordMCPPort {
		t.Fatal("request MCP listener collides with a channel MCP listener")
	}
}
