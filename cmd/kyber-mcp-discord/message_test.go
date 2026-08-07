package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitDiscordMessageRespectsUTF16Limit(t *testing.T) {
	tests := []struct {
		name    string
		content string
		min     int
	}{
		{name: "short", content: "hello", min: 1},
		{name: "long ASCII", content: strings.Repeat("word ", 1000), min: 3},
		{name: "astral emoji count as two units", content: strings.Repeat("🚀", 1500), min: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chunks := splitDiscordMessage(tc.content)
			if len(chunks) < tc.min {
				t.Fatalf("chunks = %d, want at least %d", len(chunks), tc.min)
			}
			for i, chunk := range chunks {
				if got := utf16Units(chunk); got > discordMessageLimit {
					t.Errorf("chunk %d = %d UTF-16 units, limit %d", i, got, discordMessageLimit)
				}
			}
		})
	}
}

func TestSplitDiscordMessagePreservesCodeFences(t *testing.T) {
	content := "Before\n```go\n" + strings.Repeat("fmt.Println(\"hello\")\n", 150) + "```\nAfter"
	chunks := splitDiscordMessage(content)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want split", len(chunks))
	}
	for i, chunk := range chunks {
		if strings.Count(chunk, "```")%2 != 0 {
			t.Errorf("chunk %d has unbalanced code fences:\n%s", i, chunk)
		}
	}
	if !strings.HasPrefix(chunks[1], "```go\n") {
		t.Fatalf("second chunk did not reopen Go fence: %q", chunks[1][:min(30, len(chunks[1]))])
	}
}

func TestSendDiscordTextSplitsAndRepliesOnlyOnFirstChunk(t *testing.T) {
	sender := &fakeDiscordSender{}
	out, err := sendDiscordText(sender, "123", strings.Repeat("word ", 1000), "456")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.MessageIDs) < 2 || len(sender.messages) != len(out.MessageIDs) {
		t.Fatalf("ids=%v messages=%d", out.MessageIDs, len(sender.messages))
	}
	if sender.messages[0].Reference == nil || sender.messages[0].Reference.MessageID != "456" {
		t.Fatalf("first reference = %+v", sender.messages[0].Reference)
	}
	for i, message := range sender.messages[1:] {
		if message.Reference != nil {
			t.Errorf("follow-up chunk %d unexpectedly has reference %+v", i+2, message.Reference)
		}
	}
}

func TestSendDiscordTextReportsPartialDelivery(t *testing.T) {
	sender := &fakeDiscordSender{err: fmt.Errorf("upstream failed"), errorAt: 2}
	out, err := sendDiscordText(sender, "123", strings.Repeat("word ", 1000), "")
	if err == nil || len(out.MessageIDs) != 1 || !strings.Contains(err.Error(), "chunk 2") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
