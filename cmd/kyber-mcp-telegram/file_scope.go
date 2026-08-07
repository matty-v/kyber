package main

import "sync"

const maxScopedTelegramFiles = 256

// fileSet scopes download_attachment to opaque Telegram file IDs that arrived
// from allowlisted inbound messages. A model cannot use the bot as an oracle
// for arbitrary bot-scoped file IDs it learned elsewhere.
type fileSet struct {
	mu    sync.RWMutex
	m     map[string]bool
	order []string
}

func newFileSet() *fileSet { return &fileSet{m: map[string]bool{}} }

func (s *fileSet) add(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[id] {
		return
	}
	s.m[id] = true
	s.order = append(s.order, id)
	if len(s.order) > maxScopedTelegramFiles {
		delete(s.m, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *fileSet) has(id string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[id]
}
