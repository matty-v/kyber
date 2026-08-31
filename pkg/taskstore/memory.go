package taskstore

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

type idempotencyRecord struct{ hash, taskID string }

// MemoryStore is a contract test double. Production public routes must use the
// PostgreSQL store and must never silently fall back to this implementation.
type MemoryStore struct {
	mu          sync.Mutex
	limits      Limits
	now         func() time.Time
	tasks       map[string]*Task
	idempotency map[string]idempotencyRecord
}

func NewMemoryStore(limits Limits) (*MemoryStore, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &MemoryStore{limits: limits, now: time.Now, tasks: map[string]*Task{}, idempotency: map[string]idempotencyRecord{}}, nil
}

func (s *MemoryStore) Create(_ context.Context, p CreateParams) (*CreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCreate(s.limits, p); err != nil {
		return nil, err
	}
	key := p.CreatedBy + "\x00" + p.Agent.Namespace + "\x00" + p.Agent.Name + "\x00" + p.IdempotencyKey
	if p.IdempotencyKey != "" {
		if old, ok := s.idempotency[key]; ok {
			if old.hash != p.RequestHash {
				return nil, ErrIdempotencyConflict
			}
			return &CreateResult{Task: cloneTask(s.tasks[old.taskID]), Replay: true}, nil
		}
	}
	if _, ok := s.tasks[p.ID]; ok {
		return nil, ErrConflict
	}
	if len(s.tasks) >= s.limits.MaxRetained {
		return nil, ErrCapacity
	}
	outstanding := 0
	for _, t := range s.tasks {
		if t.AgentNamespace == p.Agent.Namespace && t.AgentName == p.Agent.Name && (t.State == StateQueued || t.State == StateDispatched) {
			outstanding++
		}
	}
	if outstanding >= s.limits.MaxOutstanding {
		return nil, ErrOutstandingLimit
	}
	now := s.now().UTC()
	deadline := p.DeadlineAt.UTC()
	if deadline.IsZero() {
		deadline = now.Add(s.limits.DefaultDeadline)
	}
	if !deadline.After(now) {
		return nil, ErrInvalid
	}
	if deadline.After(now.Add(s.limits.MaxDeadline)) {
		deadline = now.Add(s.limits.MaxDeadline)
	}
	t := &Task{ID: p.ID, AgentNamespace: p.Agent.Namespace, AgentName: p.Agent.Name, CreatedBy: p.CreatedBy, Prompt: p.Prompt, Correlation: p.Correlation, State: StateQueued, Version: 1, CreatedAt: now, UpdatedAt: now, DeadlineAt: deadline, RetainUntil: deadline.Add(s.limits.Retention)}
	s.tasks[t.ID] = t
	if p.IdempotencyKey != "" {
		s.idempotency[key] = idempotencyRecord{p.RequestHash, t.ID}
	}
	return &CreateResult{Task: cloneTask(t)}, nil
}

func validateCreate(l Limits, p CreateParams) error {
	if p.ID == "" || p.Agent.Namespace == "" || p.Agent.Name == "" || p.CreatedBy == "" || strings.TrimSpace(p.Prompt) == "" {
		return ErrInvalid
	}
	if len([]byte(p.Prompt)) > l.MaxPromptBytes {
		return ErrPromptTooLarge
	}
	if len([]byte(p.Correlation)) > l.MaxCorrelationBytes {
		return ErrCorrelationTooLarge
	}
	if len([]byte(p.IdempotencyKey)) > HardMaxIdempotencyBytes {
		return ErrIdempotencyTooLarge
	}
	if p.IdempotencyKey != "" && p.RequestHash == "" {
		return ErrInvalid
	}
	return nil
}

func (s *MemoryStore) Get(_ context.Context, a AgentRef, id string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t.AgentNamespace != a.Namespace || t.AgentName != a.Name {
		return nil, ErrNotFound
	}
	return cloneTask(t), nil
}

func (s *MemoryStore) List(_ context.Context, p ListParams) (*Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := p.Limit
	if limit == 0 {
		limit = s.limits.DefaultListPage
	}
	if limit < 1 || limit > s.limits.MaxListPage {
		return nil, ErrInvalid
	}
	var before time.Time
	var beforeID string
	var err error
	if p.Cursor != "" {
		before, beforeID, err = decodeCursor(p.Cursor, p.Agent, p.State)
		if err != nil {
			return nil, err
		}
	}
	items := make([]*Task, 0)
	for _, t := range s.tasks {
		if t.AgentNamespace != p.Agent.Namespace || t.AgentName != p.Agent.Name || (p.State != "" && t.State != p.State) {
			continue
		}
		if !before.IsZero() && (t.CreatedAt.After(before) || (t.CreatedAt.Equal(before) && t.ID >= beforeID)) {
			continue
		}
		items = append(items, cloneTask(t))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	page := &Page{}
	if len(items) > limit {
		page.Tasks = items[:limit]
		page.NextCursor, err = encodeCursor(p.Agent, p.State, page.Tasks[len(page.Tasks)-1])
		if err != nil {
			return nil, err
		}
	} else {
		page.Tasks = items
	}
	return page, nil
}

func (s *MemoryStore) locked(a AgentRef, id string) (*Task, error) {
	t, ok := s.tasks[id]
	if !ok || t.AgentNamespace != a.Namespace || t.AgentName != a.Name {
		return nil, ErrNotFound
	}
	return t, nil
}

func (s *MemoryStore) MarkDispatched(_ context.Context, a AgentRef, id string, v int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return err
	}
	if t.State == StateDispatched {
		return nil
	}
	if t.Version != v || t.State != StateQueued {
		return ErrConflict
	}
	t.State = StateDispatched
	t.Version++
	t.UpdatedAt = s.now().UTC()
	return nil
}
func (s *MemoryStore) Fail(_ context.Context, a AgentRef, id string, v int64, code FailureCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return err
	}
	if t.State == StateFailed && t.FailureCode == code {
		return nil
	}
	if t.Version != v || (t.State != StateQueued && t.State != StateDispatched) {
		return ErrConflict
	}
	now := s.now().UTC()
	t.State = StateFailed
	t.FailureCode = code
	t.Version++
	t.UpdatedAt = now
	t.CompletedAt = &now
	return nil
}
func (s *MemoryStore) Complete(_ context.Context, a AgentRef, id string, v int64, response string) error {
	if len([]byte(response)) > s.limits.MaxResponseBytes {
		return ErrResponseTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return err
	}
	if t.State == StateCompleted && t.Response == response {
		return nil
	}
	if t.Version != v || t.State != StateDispatched {
		return ErrConflict
	}
	now := s.now().UTC()
	t.State = StateCompleted
	t.Response = response
	t.Version++
	t.UpdatedAt = now
	t.CompletedAt = &now
	return nil
}
