package main

import (
	"context"
	"testing"
	"time"
)

func TestDiscordLifecycleBeginAndComplete(t *testing.T) {
	api := &fakeDiscordSender{}
	l := newDiscordLifecycle(context.Background(), api, time.Hour)
	l.begin("123", "456")
	if len(api.typing) != 1 || api.typing[0] != "123" {
		t.Fatalf("typing = %v", api.typing)
	}
	if len(api.added) != 1 || api.added[0] != "123/456/👀" {
		t.Fatalf("added = %v", api.added)
	}
	l.complete("123", "456")
	if len(api.removed) != 1 || api.removed[0] != "123/456/👀/@me" {
		t.Fatalf("removed = %v", api.removed)
	}
	if len(api.added) != 2 || api.added[1] != "123/456/✅" {
		t.Fatalf("added after complete = %v", api.added)
	}
	if len(l.active) != 0 {
		t.Fatalf("active = %v, want empty", l.active)
	}
}

func TestDiscordLifecycleReplyWithoutMessageCompletesChannel(t *testing.T) {
	api := &fakeDiscordSender{}
	l := newDiscordLifecycle(context.Background(), api, time.Hour)
	l.begin("123", "a")
	l.begin("123", "b")
	l.begin("999", "c")
	l.complete("123", "")
	if len(l.active) != 1 || l.active[lifecycleKey("999", "c")] == nil {
		t.Fatalf("active = %v, want only other channel", l.active)
	}
}

func TestDiscordLifecycleFailedMarksMessage(t *testing.T) {
	api := &fakeDiscordSender{}
	l := newDiscordLifecycle(context.Background(), api, time.Hour)
	l.failed("123", "456")
	if len(api.added) != 1 || api.added[0] != "123/456/❌" {
		t.Fatalf("added = %v", api.added)
	}
}

func TestDiscordMCPReplyCompletesLifecycle(t *testing.T) {
	api := &fakeDiscordSender{}
	l := newDiscordLifecycle(context.Background(), api, time.Hour)
	l.begin("123", "456")
	s := &discordMCPServer{sender: api, allowedChannels: map[string]bool{"123": true}, lifecycle: l}
	discordRPC(t, s, "tools/call", map[string]any{
		"name": "reply", "arguments": map[string]any{"channel_id": "123", "text": "done", "message_id": "456"},
	})
	if len(l.active) != 0 || len(api.removed) != 1 {
		t.Fatalf("active=%v removed=%v", l.active, api.removed)
	}
}
