package briefstore

import (
	"context"
	"sync"
)

// MemoryStore is an in-memory BriefStore for unit tests and local development.
// It uses a read-write mutex to allow concurrent reads.
type MemoryStore struct {
	mu     sync.RWMutex
	briefs map[string]*Brief
}

// NewMemoryStore returns a new, empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		briefs: make(map[string]*Brief),
	}
}

// Put stores a defensive copy of the brief for the given agent, replacing any existing entry.
// Copying on write prevents the caller from mutating the stored value after Put returns.
func (m *MemoryStore) Put(_ context.Context, agentName string, brief *Brief) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := *brief
	if brief.RecentExchanges != nil {
		stored.RecentExchanges = append([]Exchange(nil), brief.RecentExchanges...)
	}
	m.briefs[agentName] = &stored
	return nil
}

// Get returns a copy of the stored brief for agentName, or ErrBriefNotFound if not present.
// Returning a copy prevents callers from mutating the stored value.
func (m *MemoryStore) Get(_ context.Context, agentName string) (*Brief, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.briefs[agentName]
	if !ok {
		return nil, ErrBriefNotFound
	}
	// Return a copy to prevent callers from mutating the stored value.
	out := *b
	if b.RecentExchanges != nil {
		out.RecentExchanges = append([]Exchange(nil), b.RecentExchanges...)
	}
	return &out, nil
}

// Delete removes the brief for agentName. Safe to call when no brief exists.
func (m *MemoryStore) Delete(_ context.Context, agentName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.briefs, agentName)
	return nil
}
