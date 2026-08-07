package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	workingReaction = "👀"
	doneReaction    = "✅"
	failedReaction  = "❌"
)

type discordLifecycleAPI interface {
	ChannelTyping(channelID string, options ...discordgo.RequestOption) error
	MessageReactionAdd(channelID, messageID, emojiID string, options ...discordgo.RequestOption) error
	MessageReactionRemove(channelID, messageID, emojiID, userID string, options ...discordgo.RequestOption) error
}

// discordLifecycle makes an accepted Discord turn visible while the runtime
// works. Discord typing expires after several seconds, so one goroutine per
// active message refreshes it until either outbound path sends the reply.
type discordLifecycle struct {
	ctx      context.Context
	api      discordLifecycleAPI
	interval time.Duration

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func newDiscordLifecycle(ctx context.Context, api discordLifecycleAPI, interval time.Duration) *discordLifecycle {
	return &discordLifecycle{ctx: ctx, api: api, interval: interval, active: map[string]context.CancelFunc{}}
}

func lifecycleKey(channelID, messageID string) string { return channelID + "/" + messageID }

func (l *discordLifecycle) begin(channelID, messageID string) {
	if l == nil || l.api == nil || channelID == "" || messageID == "" {
		return
	}
	key := lifecycleKey(channelID, messageID)
	ctx, cancel := context.WithCancel(l.ctx)
	l.mu.Lock()
	if previous := l.active[key]; previous != nil {
		previous()
	}
	l.active[key] = cancel
	l.mu.Unlock()

	l.addReaction(channelID, messageID, workingReaction)
	l.sendTyping(channelID)
	go func() {
		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.sendTyping(channelID)
			}
		}
	}()
}

func (l *discordLifecycle) complete(channelID, messageID string) {
	if l == nil || l.api == nil || channelID == "" {
		return
	}
	for _, id := range l.stop(channelID, messageID) {
		if err := l.api.MessageReactionRemove(channelID, id, workingReaction, "@me"); err != nil {
			slog.Debug("discord-sidecar: remove working reaction failed", "channel", channelID, "message_id", id, "error", err)
		}
		l.addReaction(channelID, id, doneReaction)
	}
}

func (l *discordLifecycle) failed(channelID, messageID string) {
	if l == nil || l.api == nil || channelID == "" || messageID == "" {
		return
	}
	l.stop(channelID, messageID)
	l.addReaction(channelID, messageID, failedReaction)
}

// stop cancels an exact message when supplied. A reply without message_id
// completes every active turn in that channel so typing cannot leak forever.
func (l *discordLifecycle) stop(channelID, messageID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var stopped []string
	for key, cancel := range l.active {
		candidateChannel, candidateMessage := splitLifecycleKey(key)
		if candidateChannel != channelID || (messageID != "" && candidateMessage != messageID) {
			continue
		}
		cancel()
		delete(l.active, key)
		stopped = append(stopped, candidateMessage)
	}
	return stopped
}

func splitLifecycleKey(key string) (string, string) {
	for i := range key {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

func (l *discordLifecycle) sendTyping(channelID string) {
	if err := l.api.ChannelTyping(channelID); err != nil {
		slog.Debug("discord-sidecar: typing indicator failed", "channel", channelID, "error", err)
	}
}

func (l *discordLifecycle) addReaction(channelID, messageID, emoji string) {
	if err := l.api.MessageReactionAdd(channelID, messageID, emoji); err != nil {
		slog.Debug("discord-sidecar: lifecycle reaction failed", "channel", channelID, "message_id", messageID, "emoji", emoji, "error", err)
	}
}
