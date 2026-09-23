package archivejob

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

// MemoryStore is a Store for tests. Jobs are deep-copied in and out so a
// caller mutating its copy cannot bypass the version check.
type MemoryStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{jobs: map[string]*Job{}} }

func clone(j *Job) *Job {
	b, _ := json.Marshal(j)
	var out Job
	_ = json.Unmarshal(b, &out)
	return &out
}

func (s *MemoryStore) Create(ctx context.Context, j *Job, maxActive int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := 0
	for _, x := range s.jobs {
		if x.State.Terminal() {
			continue
		}
		active++
		if j.Agent != "" && x.Agent == j.Agent && x.Kind == j.Kind {
			return ErrActiveJob
		}
	}
	if maxActive > 0 && active >= maxActive {
		return ErrConcurrency
	}
	j.Version = 1
	s.jobs[j.ID] = clone(j)
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(j), nil
}

func (s *MemoryStore) List(ctx context.Context, kind Kind, agent string, limit int) ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Job
	for _, j := range s.jobs {
		if j.Kind == kind && (agent == "" || j.Agent == agent) {
			out = append(out, clone(j))
		}
	}
	slices.SortFunc(out, func(a, b *Job) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) ListActive(ctx context.Context, now time.Time) ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Job
	for _, j := range s.jobs {
		if !j.State.Terminal() || j.State == StateCompleted && j.ExpiresAt != nil && !j.ExpiresAt.After(now) {
			out = append(out, clone(j))
		}
	}
	slices.SortFunc(out, func(a, b *Job) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

func (s *MemoryStore) Update(ctx context.Context, j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.jobs[j.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Version != j.Version {
		return ErrConflict
	}
	j.Version++
	j.UpdatedAt = time.Now().UTC()
	s.jobs[j.ID] = clone(j)
	return nil
}
