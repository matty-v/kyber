package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	albumSettleDelay = 600 * time.Millisecond
	maxPendingAlbums = 32
	maxAlbumItems    = 10
	maxAlbumAttempts = 5
	albumRetryDelay  = 2 * time.Second
)

type albumAttachment struct {
	Type      string `json:"type"`
	FileID    string `json:"file_id"`
	Name      string `json:"name,omitempty"`
	MessageID string `json:"message_id"`
}

type pendingAlbum struct {
	messages []*telegramMessage
	timer    *time.Timer
}

type albumCollector struct {
	mu         sync.Mutex
	ctx        context.Context
	cfg        *config
	client     *http.Client
	items      map[string]*pendingAlbum
	order      []string
	retryDelay time.Duration
	closed     bool
}

func newAlbumCollector(ctx context.Context, cfg *config, client *http.Client) *albumCollector {
	return &albumCollector{ctx: ctx, cfg: cfg, client: client, items: map[string]*pendingAlbum{}, retryDelay: albumRetryDelay}
}

func (c *albumCollector) add(message telegramMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	album := c.items[message.MediaGroupID]
	if album == nil {
		album = &pendingAlbum{}
		c.items[message.MediaGroupID] = album
		c.order = append(c.order, message.MediaGroupID)
		if len(c.order) > maxPendingAlbums {
			c.flushLocked(c.order[0])
		}
	}
	if len(album.messages) < maxAlbumItems {
		copy := message
		album.messages = append(album.messages, &copy)
	}
	if album.timer != nil {
		album.timer.Stop()
	}
	groupID := message.MediaGroupID
	album.timer = time.AfterFunc(albumSettleDelay, func() { c.flush(groupID) })
}

func (c *albumCollector) flush(groupID string) {
	c.mu.Lock()
	c.flushLocked(groupID)
	c.mu.Unlock()
}

func (c *albumCollector) flushLocked(groupID string) {
	album := c.items[groupID]
	if album == nil {
		return
	}
	delete(c.items, groupID)
	for i, id := range c.order {
		if id == groupID {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	if album.timer != nil {
		album.timer.Stop()
	}
	messages := album.messages
	go func() {
		var err error
		for attempt := 1; attempt <= maxAlbumAttempts; attempt++ {
			err = forwardMessages(c.ctx, c.cfg, c.client, messages, false)
			if err == nil || c.ctx.Err() != nil {
				return
			}
			if attempt == maxAlbumAttempts {
				break
			}
			slog.Warn("telegram-sidecar: forward album failed; retrying", "media_group_id", groupID,
				"attempt", attempt, "error", err)
			timer := time.NewTimer(c.retryDelay)
			select {
			case <-c.ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
		slog.Error("telegram-sidecar: dropping album after repeated failures", "media_group_id", groupID,
			"attempts", maxAlbumAttempts, "error", err)
	}()
}

func (c *albumCollector) close() {
	c.mu.Lock()
	c.closed = true
	ids := append([]string(nil), c.order...)
	for _, id := range ids {
		c.flushLocked(id)
	}
	c.mu.Unlock()
}
