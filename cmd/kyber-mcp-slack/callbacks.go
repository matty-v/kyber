package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
)

const maxSlackCallbacks = 256

type slackCallbackEntry struct {
	ChannelID string
	MessageID string
	Label     string
	Value     string
}
type slackCallbackRegistry struct {
	mu    sync.Mutex
	items map[string]slackCallbackEntry
	order []string
}

func newSlackCallbackRegistry() *slackCallbackRegistry {
	return &slackCallbackRegistry{items: map[string]slackCallbackEntry{}}
}

func (r *slackCallbackRegistry) register(channelID string, value any) ([]map[string]any, []string, error) {
	raw, _ := value.([]any)
	if value != nil && raw == nil {
		return nil, nil, fmt.Errorf("buttons must be an array")
	}
	if len(raw) > 100 {
		return nil, nil, fmt.Errorf("at most 100 buttons are allowed")
	}
	elements := make([]map[string]any, 0, len(raw))
	tokens := make([]string, 0, len(raw))
	for _, item := range raw {
		button, _ := item.(map[string]any)
		label, _ := button["text"].(string)
		buttonValue, _ := button["value"].(string)
		if label == "" || buttonValue == "" {
			r.remove(tokens)
			return nil, nil, fmt.Errorf("button text and value are required")
		}
		if len([]rune(label)) > 75 || len(buttonValue) > 1024 {
			r.remove(tokens)
			return nil, nil, fmt.Errorf("button text or value exceeds Slack limits")
		}
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			r.remove(tokens)
			return nil, nil, fmt.Errorf("generating callback token: %w", err)
		}
		token := hex.EncodeToString(buf)
		r.mu.Lock()
		r.items[token] = slackCallbackEntry{ChannelID: channelID, Label: label, Value: buttonValue}
		r.order = append(r.order, token)
		for len(r.order) > maxSlackCallbacks {
			delete(r.items, r.order[0])
			r.order = r.order[1:]
		}
		r.mu.Unlock()
		tokens = append(tokens, token)
		elements = append(elements, map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": label}, "value": token, "action_id": token})
	}
	if len(elements) == 0 {
		return nil, nil, nil
	}
	return []map[string]any{{"type": "actions", "elements": elements}}, tokens, nil
}

func (r *slackCallbackRegistry) consume(token, channelID string) (slackCallbackEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.items[token]
	if !ok || entry.ChannelID != channelID {
		return slackCallbackEntry{}, false
	}
	delete(r.items, token)
	return entry, true
}

func (r *slackCallbackRegistry) bind(tokens []string, messageID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, token := range tokens {
		entry, ok := r.items[token]
		if ok {
			entry.MessageID = messageID
			r.items[token] = entry
		}
	}
}

func (r *slackCallbackRegistry) remove(tokens []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, token := range tokens {
		delete(r.items, token)
	}
}
func (r *slackCallbackRegistry) removeForMessage(channelID, messageID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for token, entry := range r.items {
		if entry.ChannelID == channelID && entry.MessageID == messageID {
			delete(r.items, token)
		}
	}
}
