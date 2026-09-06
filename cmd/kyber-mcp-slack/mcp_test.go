package main

import "testing"

func TestSlackMCPChannelAllowlist(t *testing.T) {
	s := newMCPServer(config{channels: map[string]bool{"C123": true}}, nil)
	if !s.channelAllowed("C123") { t.Fatal("allowlisted channel was rejected") }
	if s.channelAllowed("C999") { t.Fatal("unallowlisted channel was accepted") }
	if newMCPServer(config{}, nil).channelAllowed("C123") { t.Fatal("empty allowlist must fail closed") }
}
