package statechangestore

import (
	"context"
	"sync"
)

// MemoryAccumulator is an in-memory Accumulator for development installs.
// Data evaporates on restart and is not shared across replicas.
type MemoryAccumulator struct {
	mu     sync.Mutex
	counts map[string]map[string]int64 // namespace:agent -> toState -> count
}

// NewMemoryAccumulator returns an empty MemoryAccumulator.
func NewMemoryAccumulator() *MemoryAccumulator {
	return &MemoryAccumulator{counts: map[string]map[string]int64{}}
}

// IncrBy increments the transition count for (namespace, agent, toState).
func (m *MemoryAccumulator) IncrBy(_ context.Context, namespace, agentName, toState string, delta int64) error {
	if delta <= 0 {
		return nil
	}
	key := namespace + ":" + agentName
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts[key] == nil {
		m.counts[key] = map[string]int64{}
	}
	m.counts[key][toState] += delta
	return nil
}

// DeleteAgent removes the state-change counts for (namespace, agentName).
// Idempotent — a missing key is a no-op (kyber#565).
func (m *MemoryAccumulator) DeleteAgent(_ context.Context, namespace, agentName string) error {
	key := namespace + ":" + agentName
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.counts, key)
	return nil
}

// GetAll returns counts for all (agent, toState) pairs in namespace.
func (m *MemoryAccumulator) GetAll(_ context.Context, namespace string) ([]AgentStateCounts, error) {
	prefix := namespace + ":"
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AgentStateCounts
	for key, states := range m.counts {
		if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		agentName := key[len(prefix):]
		for toState, count := range states {
			out = append(out, AgentStateCounts{
				Agent:   agentName,
				ToState: toState,
				Count:   count,
			})
		}
	}
	return out, nil
}
