package messagebuffer

import (
	"context"
	"sync"
)

// MemoryBuffer is an in-memory MessageBuffer for unit tests and local development.
// It has no TTL — messages are retained until Drain is called.
// It is safe for concurrent use.
type MemoryBuffer struct {
	mu   sync.Mutex
	msgs map[string][]PendingMessage
}

// NewMemoryBuffer returns a new, empty MemoryBuffer.
func NewMemoryBuffer() *MemoryBuffer {
	return &MemoryBuffer{
		msgs: make(map[string][]PendingMessage),
	}
}

// Push appends msg to the buffer for agentName.
func (m *MemoryBuffer) Push(_ context.Context, agentName string, msg PendingMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs[agentName] = append(m.msgs[agentName], msg)
	return nil
}

// Drain removes and returns all buffered messages for agentName.
// Returns an empty (non-nil) slice if no messages are buffered.
func (m *MemoryBuffer) Drain(_ context.Context, agentName string) ([]PendingMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs := m.msgs[agentName]
	if len(msgs) == 0 {
		return []PendingMessage{}, nil
	}
	// Return a copy and clear the buffer atomically.
	out := make([]PendingMessage, len(msgs))
	copy(out, msgs)
	delete(m.msgs, agentName)
	return out, nil
}

// Delete discards any buffered messages for agentName. Idempotent — a missing
// key is a no-op (kyber#565).
func (m *MemoryBuffer) Delete(_ context.Context, agentName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.msgs, agentName)
	return nil
}
