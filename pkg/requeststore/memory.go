package requeststore

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is the single-process development and test implementation.
type MemoryStore struct {
	mu       sync.Mutex
	requests map[string]map[string]*Request
	limits   Limits
	now      func() time.Time
}

// NewMemoryStore returns an empty store using limits.
func NewMemoryStore(limits Limits) (*MemoryStore, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &MemoryStore{
		requests: make(map[string]map[string]*Request),
		limits:   limits,
		now:      time.Now,
	}, nil
}

// Create atomically enforces the per-agent outstanding limit and stores a new request.
func (m *MemoryStore) Create(_ context.Context, agent, id, prompt, correlation string) (*Request, error) {
	if err := validateCreate(agent, id, prompt, correlation, m.limits); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.pruneLocked(agent, now)
	requests := m.requests[agent]
	if requests == nil {
		requests = make(map[string]*Request)
		m.requests[agent] = requests
	}
	if _, exists := requests[id]; exists {
		return nil, ErrConflict
	}
	outstanding := 0
	for _, request := range requests {
		if request.Status == StatusQueued || request.Status == StatusDispatched {
			outstanding++
		}
	}
	if outstanding >= m.limits.MaxOutstanding {
		return nil, ErrOutstandingLimit
	}
	request := &Request{
		ID: id, Agent: agent, Prompt: prompt, Correlation: correlation,
		Status: StatusQueued, CreatedAt: now, ExpiresAt: now.Add(m.limits.Lifetime), UpdatedAt: now,
	}
	requests[id] = request
	return clone(request), nil
}

// Get returns a defensive copy of a live request.
func (m *MemoryStore) Get(_ context.Context, agent, id string) (*Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.pruneLocked(agent, now)
	request := m.requests[agent][id]
	if request == nil {
		return nil, ErrNotFound
	}
	return clone(request), nil
}

// MarkDispatched atomically moves a queued request to dispatched.
func (m *MemoryStore) MarkDispatched(_ context.Context, agent, id string) error {
	return m.transition(agent, id, func(request *Request, now time.Time) error {
		if request.Status == StatusDispatched {
			return nil
		}
		if request.Status != StatusQueued {
			return ErrConflict
		}
		request.Status = StatusDispatched
		request.UpdatedAt = now
		return nil
	})
}

// Fail atomically moves an outstanding request to failed.
func (m *MemoryStore) Fail(_ context.Context, agent, id string, code FailureCode) error {
	if code == "" {
		return ErrInvalidRequest
	}
	return m.transition(agent, id, func(request *Request, now time.Time) error {
		if request.Status == StatusFailed && request.FailureCode == code {
			return nil
		}
		if request.Status != StatusQueued && request.Status != StatusDispatched {
			return ErrConflict
		}
		request.Status = StatusFailed
		request.FailureCode = code
		request.UpdatedAt = now
		m.trimTerminalLocked(agent)
		return nil
	})
}

// Complete atomically records the first response to a dispatched request.
func (m *MemoryStore) Complete(_ context.Context, agent, id, response string) error {
	if len([]byte(response)) > m.limits.MaxResponseBytes {
		return ErrResponseTooLarge
	}
	return m.transition(agent, id, func(request *Request, now time.Time) error {
		if request.Status == StatusCompleted {
			if request.Response == response {
				return nil
			}
			return ErrConflict
		}
		if request.Status != StatusDispatched {
			return ErrConflict
		}
		request.Status = StatusCompleted
		request.Response = response
		request.UpdatedAt = now
		m.trimTerminalLocked(agent)
		return nil
	})
}

func (m *MemoryStore) transition(agent, id string, apply func(*Request, time.Time) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.pruneLocked(agent, now)
	request := m.requests[agent][id]
	if request == nil {
		return ErrNotFound
	}
	return apply(request, now)
}

func (m *MemoryStore) pruneLocked(agent string, now time.Time) {
	requests := m.requests[agent]
	for id, request := range requests {
		if !request.ExpiresAt.After(now) {
			delete(requests, id)
		}
	}
	if len(requests) == 0 {
		delete(m.requests, agent)
	}
}

func (m *MemoryStore) trimTerminalLocked(agent string) {
	requests := m.requests[agent]
	for {
		terminal := 0
		var oldest *Request
		for _, request := range requests {
			if request.Status != StatusCompleted && request.Status != StatusFailed {
				continue
			}
			terminal++
			if oldest == nil || request.UpdatedAt.Before(oldest.UpdatedAt) ||
				(request.UpdatedAt.Equal(oldest.UpdatedAt) && request.ID < oldest.ID) {
				oldest = request
			}
		}
		if terminal <= m.limits.MaxTerminal {
			return
		}
		delete(requests, oldest.ID)
	}
}

func clone(request *Request) *Request {
	copy := *request
	return &copy
}

var _ Store = (*MemoryStore)(nil)
