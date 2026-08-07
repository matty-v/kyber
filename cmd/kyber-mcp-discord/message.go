package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

const (
	discordMessageLimit = 2000
	// Leave room to close and reopen a fenced code block around a split. The
	// longest preserved language tag is capped below, so 100 units is ample.
	discordChunkTarget = 1900
)

type discordSendOutcome struct {
	MessageIDs []string
}

// sendDiscordText applies Discord's message limit in one place for both MCP
// and the compatibility /send endpoint. Only the first chunk carries the reply
// reference; the rest follow in-order without repeatedly pinging the author.
func sendDiscordText(sender discordSender, channelID, content, replyTo string) (discordSendOutcome, error) {
	return sendDiscordMessage(sender, channelID, content, replyTo, nil)
}

func sendDiscordMessage(sender discordSender, channelID, content, replyTo string, paths []string) (discordSendOutcome, error) {
	chunks := splitDiscordMessage(content)
	out := discordSendOutcome{MessageIDs: make([]string, 0, len(chunks))}
	for i, chunk := range chunks {
		message := &discordgo.MessageSend{Content: chunk}
		if i == 0 && replyTo != "" {
			message.Reference = &discordgo.MessageReference{MessageID: replyTo, ChannelID: channelID}
		}
		var opened []*os.File
		if i == 0 {
			for _, path := range paths {
				file, err := os.Open(path)
				if err != nil {
					for _, existing := range opened {
						_ = existing.Close()
					}
					return out, fmt.Errorf("opening attachment %s: %w", filepath.Base(path), err)
				}
				opened = append(opened, file)
				message.Files = append(message.Files, &discordgo.File{Name: filepath.Base(path), Reader: file})
			}
		}
		sent, err := sender.ChannelMessageSendComplex(channelID, message)
		for _, file := range opened {
			_ = file.Close()
		}
		if err != nil {
			return out, fmt.Errorf("sending chunk %d of %d: %w", i+1, len(chunks), err)
		}
		if sent == nil || sent.ID == "" {
			return out, fmt.Errorf("sending chunk %d of %d: Discord returned no message ID", i+1, len(chunks))
		}
		out.MessageIDs = append(out.MessageIDs, sent.ID)
	}
	return out, nil
}

func splitDiscordMessage(content string) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	raw := splitByUTF16Limit(content, discordChunkTarget)
	chunks := make([]string, 0, len(raw))
	open, language := false, ""
	for _, part := range raw {
		chunk := part
		if open {
			chunk = "```" + language + "\n" + chunk
		}
		open, language = scanFenceState(part, open, language)
		if open {
			chunk += "\n```"
		}
		chunk = strings.TrimRightFunc(chunk, unicode.IsSpace)
		if utf16Units(chunk) > discordMessageLimit {
			// Defensive fallback for an unexpectedly long fence marker. The
			// raw target leaves ample headroom, so this should be unreachable.
			chunks = append(chunks, splitByUTF16Limit(chunk, discordMessageLimit)...)
			continue
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

func splitByUTF16Limit(content string, limit int) []string {
	var chunks []string
	for utf16Units(content) > limit {
		units, cut, lastBreak := 0, 0, 0
		for index, r := range content {
			width := 1
			if r > 0xffff {
				width = 2
			}
			if units+width > limit {
				cut = index
				break
			}
			units += width
			cut = index + utf8.RuneLen(r)
			if unicode.IsSpace(r) {
				lastBreak = cut
			}
		}
		if lastBreak > 0 && lastBreak >= cut/2 {
			cut = lastBreak
		}
		chunks = append(chunks, strings.TrimRightFunc(content[:cut], unicode.IsSpace))
		content = strings.TrimLeftFunc(content[cut:], unicode.IsSpace)
	}
	if content != "" {
		chunks = append(chunks, content)
	}
	return chunks
}

func utf16Units(s string) int {
	units := 0
	for _, r := range s {
		units++
		if r > 0xffff {
			units++
		}
	}
	return units
}

// scanFenceState recognizes ordinary line-oriented Markdown fences. Language
// tags are capped so a maliciously long marker cannot defeat the chunk limit.
func scanFenceState(content string, open bool, language string) (bool, string) {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		if open {
			open, language = false, ""
			continue
		}
		open = true
		language = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
		if len(language) > 32 {
			language = language[:32]
		}
	}
	return open, language
}
