package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	telegramChatActionRefresh = 4 * time.Second
	telegramChatActionMaxAge  = 5 * time.Minute
)

type chatActionLease struct {
	id     uint64
	cancel context.CancelFunc
}

// chatActionManager keeps Telegram's ephemeral typing indicator alive while an
// inbound message is being handled. Telegram expires sendChatAction after a
// few seconds, so one call is not enough for agent work that may take minutes.
type chatActionManager struct {
	ctx         context.Context
	cancel      context.CancelFunc
	cfg         *config
	client      *http.Client
	refresh     time.Duration
	maxAge      time.Duration
	mu          sync.Mutex
	nextID      uint64
	activeLease map[string]chatActionLease
}

func newChatActionManager(parent context.Context, cfg *config, client *http.Client) *chatActionManager {
	ctx, cancel := context.WithCancel(parent)
	return &chatActionManager{
		ctx: ctx, cancel: cancel, cfg: cfg, client: client,
		refresh: telegramChatActionRefresh, maxAge: telegramChatActionMaxAge,
		activeLease: map[string]chatActionLease{},
	}
}

func (m *chatActionManager) start(chatID string) {
	if m == nil || chatID == "" {
		return
	}
	m.stop(chatID)

	ctx, cancel := context.WithTimeout(m.ctx, m.maxAge)
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.activeLease[chatID] = chatActionLease{id: id, cancel: cancel}
	m.mu.Unlock()

	go m.refreshUntilDone(ctx, chatID, id)
}

func (m *chatActionManager) refreshUntilDone(ctx context.Context, chatID string, id uint64) {
	ticker := time.NewTicker(m.refresh)
	defer ticker.Stop()
	defer m.remove(chatID, id)

	for {
		if err := sendChatAction(ctx, m.cfg, m.client, chatID, "typing"); err != nil && ctx.Err() == nil {
			slog.Warn("telegram-sidecar: typing indicator failed", "chat_id", chatID, "error", err)
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				slog.Info("telegram-sidecar: typing indicator expired", "chat_id", chatID, "max_age", m.maxAge)
			}
			return
		case <-ticker.C:
		}
	}
}

func (m *chatActionManager) stop(chatID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	lease, ok := m.activeLease[chatID]
	if ok {
		delete(m.activeLease, chatID)
	}
	m.mu.Unlock()
	if ok {
		lease.cancel()
	}
}

func (m *chatActionManager) remove(chatID string, id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease, ok := m.activeLease[chatID]; ok && lease.id == id {
		delete(m.activeLease, chatID)
	}
}

func (m *chatActionManager) active(chatID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.activeLease[chatID]
	return ok
}

func (m *chatActionManager) close() {
	if m != nil {
		m.cancel()
	}
}
