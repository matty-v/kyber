package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

const (
	maxTelegramCallbacks     = 256
	maxTelegramButtons       = 100
	maxTelegramButtonRunes   = 64
	maxTelegramCallbackValue = 1024
)

type callbackButton struct {
	Text  string
	Value string
}

type callbackEntry struct {
	Token     string
	ChatID    string
	MessageID string
	Label     string
	Value     string
}

func (r *callbackRegistry) bindMessage(tokens []string, messageID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, token := range tokens {
		entry, ok := r.items[token]
		if !ok {
			continue
		}
		entry.MessageID = messageID
		r.items[token] = entry
	}
}

func (r *callbackRegistry) removeForMessage(chatID, messageID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for token, entry := range r.items {
		if entry.ChatID == chatID && entry.MessageID == messageID {
			delete(r.items, token)
		}
	}
}

type callbackRegistry struct {
	mu    sync.Mutex
	items map[string]callbackEntry
	order []string
}

func newCallbackRegistry() *callbackRegistry {
	return &callbackRegistry{items: map[string]callbackEntry{}}
}

func (r *callbackRegistry) register(chatID string, buttons []callbackButton) (string, []string, error) {
	if len(buttons) > maxTelegramButtons {
		return "", nil, fmt.Errorf("at most %d buttons are allowed", maxTelegramButtons)
	}
	keyboard := make([][]map[string]string, 0, len(buttons))
	tokens := make([]string, 0, len(buttons))
	for _, button := range buttons {
		if button.Text == "" || button.Value == "" {
			r.remove(tokens)
			return "", nil, fmt.Errorf("button text and value are required")
		}
		if len([]rune(button.Text)) > maxTelegramButtonRunes {
			r.remove(tokens)
			return "", nil, fmt.Errorf("button text must be at most %d characters", maxTelegramButtonRunes)
		}
		if len(button.Value) > maxTelegramCallbackValue {
			r.remove(tokens)
			return "", nil, fmt.Errorf("button value must be at most %d bytes", maxTelegramCallbackValue)
		}
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			r.remove(tokens)
			return "", nil, fmt.Errorf("generating callback token: %w", err)
		}
		token := hex.EncodeToString(buf)
		r.mu.Lock()
		r.items[token] = callbackEntry{Token: token, ChatID: chatID, Label: button.Text, Value: button.Value}
		r.order = append(r.order, token)
		for len(r.order) > maxTelegramCallbacks {
			delete(r.items, r.order[0])
			r.order = r.order[1:]
		}
		r.mu.Unlock()
		tokens = append(tokens, token)
		keyboard = append(keyboard, []map[string]string{{"text": button.Text, "callback_data": token}})
	}
	raw, err := json.Marshal(map[string]any{"inline_keyboard": keyboard})
	if err != nil {
		r.remove(tokens)
		return "", nil, fmt.Errorf("encoding inline keyboard: %w", err)
	}
	return string(raw), tokens, nil
}

func (r *callbackRegistry) get(token, chatID string) (callbackEntry, bool) {
	if r == nil {
		return callbackEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.items[token]
	if !ok || entry.ChatID != chatID {
		return callbackEntry{}, false
	}
	return entry, true
}

func (r *callbackRegistry) consume(token, chatID string) (callbackEntry, bool) {
	if r == nil {
		return callbackEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.items[token]
	if !ok || entry.ChatID != chatID {
		return callbackEntry{}, false
	}
	delete(r.items, token)
	return entry, true
}

func (r *callbackRegistry) remove(tokens []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, token := range tokens {
		delete(r.items, token)
	}
}
