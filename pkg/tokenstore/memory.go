package tokenstore

import (
	"context"
	"sync"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

// MemoryStore is an in-memory TokenStore for tests and local dev. It has
// no TTL — entries are retained until the process exits.
type MemoryStore struct {
	mu    sync.RWMutex
	items map[string]*tokenreport.Snapshot
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string]*tokenreport.Snapshot)}
}

// Put stores snap under agentName.
func (m *MemoryStore) Put(_ context.Context, agentName string, snap *tokenreport.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[agentName] = snap
	return nil
}

// Get returns the snapshot for agentName, or nil if absent.
func (m *MemoryStore) Get(_ context.Context, agentName string) (*tokenreport.Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.items[agentName], nil
}

// Delete removes the snapshot for agentName. Idempotent — a missing key is a
// no-op (kyber#565).
func (m *MemoryStore) Delete(_ context.Context, agentName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, agentName)
	return nil
}
