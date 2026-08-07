package tokenstore

import (
	"context"
	"strings"
	"sync"
)

// MemoryAccumulator is an in-memory Accumulator for tests and local dev.
// It is safe for concurrent use and has no TTL.
type MemoryAccumulator struct {
	mu   sync.Mutex
	data map[string]AgentModelCounts // key: "{namespace}:{agent}:{model}"
}

// NewMemoryAccumulator returns an empty MemoryAccumulator.
func NewMemoryAccumulator() *MemoryAccumulator {
	return &MemoryAccumulator{data: map[string]AgentModelCounts{}}
}

// IncrBy increments the counts for (namespace, agentName, model) by delta.
func (m *MemoryAccumulator) IncrBy(_ context.Context, namespace, agentName, model string, delta TokenDelta) error {
	if delta.IsZero() {
		return nil
	}
	key := namespace + ":" + agentName + ":" + model
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.data[key]
	cur.Agent = agentName
	cur.Model = model
	cur.Input += delta.Input
	cur.CacheCreation += delta.CacheCreation
	cur.CacheRead += delta.CacheRead
	cur.Output += delta.Output
	m.data[key] = cur
	return nil
}

// GetAll returns all counts for the given namespace.
func (m *MemoryAccumulator) GetAll(_ context.Context, namespace string) ([]AgentModelCounts, error) {
	prefix := namespace + ":"
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AgentModelCounts, 0)
	for k, v := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, v)
		}
	}
	return out, nil
}

// DeleteAgent removes all (model) entries for (namespace, agentName). Idempotent
// — no entries is a no-op (kyber#565). The key is "{namespace}:{agent}:{model}",
// so we match on the "{namespace}:{agent}:" prefix to avoid deleting another
// agent whose name is a prefix of this one.
func (m *MemoryAccumulator) DeleteAgent(_ context.Context, namespace, agentName string) error {
	prefix := namespace + ":" + agentName + ":"
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			delete(m.data, k)
		}
	}
	return nil
}
