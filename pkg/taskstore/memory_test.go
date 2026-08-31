package taskstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testStore(t *testing.T) *MemoryStore {
	t.Helper()
	s, err := NewMemoryStore(DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s
}

func createTask(t *testing.T, s *MemoryStore, id, key, hash string) *CreateResult {
	t.Helper()
	r, err := s.Create(context.Background(), CreateParams{ID: id, Agent: AgentRef{Namespace: "kyber-system", Name: "sol"}, CreatedBy: "operator", Prompt: "ship it", IdempotencyKey: key, RequestHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMemoryCreateIdempotencyAndIsolation(t *testing.T) {
	s := testStore(t)
	first := createTask(t, s, "task_00000000000000000000000000000001", "same", "hash")
	if first.Replay || first.Task.State != StateQueued || first.Task.Version != 1 {
		t.Fatalf("unexpected first result: %+v", first)
	}
	replay := createTask(t, s, "task_unused", "same", "hash")
	if !replay.Replay || replay.Task.ID != first.Task.ID {
		t.Fatalf("unexpected replay: %+v", replay)
	}
	_, err := s.Create(context.Background(), CreateParams{ID: "task_other", Agent: AgentRef{Namespace: "kyber-system", Name: "sol"}, CreatedBy: "operator", Prompt: "different", IdempotencyKey: "same", RequestHash: "different"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("got %v", err)
	}
	_, err = s.Get(context.Background(), AgentRef{Namespace: "kyber-system", Name: "other"}, first.Task.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-agent get = %v", err)
	}
}

func TestMemoryTransitionsSingleAssignment(t *testing.T) {
	s := testStore(t)
	r := createTask(t, s, "task_00000000000000000000000000000001", "", "")
	if err := s.MarkDispatched(context.Background(), AgentRef{"kyber-system", "sol"}, r.Task.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(context.Background(), AgentRef{"kyber-system", "sol"}, r.Task.ID, 2, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(context.Background(), AgentRef{"kyber-system", "sol"}, r.Task.ID, 2, "done"); err != nil {
		t.Fatalf("identical completion not idempotent: %v", err)
	}
	if err := s.Complete(context.Background(), AgentRef{"kyber-system", "sol"}, r.Task.ID, 3, "different"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different completion = %v", err)
	}
	if err := s.Fail(context.Background(), AgentRef{"kyber-system", "sol"}, r.Task.ID, 3, FailureInternal); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal overwrite = %v", err)
	}
}

func TestMemoryListStableCursorAndFilterBinding(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"task_1", "task_2", "task_3"} {
		createTask(t, s, id, "", "")
	}
	p1, err := s.List(context.Background(), ListParams{Agent: AgentRef{"kyber-system", "sol"}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Tasks) != 2 || p1.Tasks[0].ID != "task_3" || p1.NextCursor == "" {
		t.Fatalf("page 1: %+v", p1)
	}
	p2, err := s.List(context.Background(), ListParams{Agent: AgentRef{"kyber-system", "sol"}, Limit: 2, Cursor: p1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Tasks) != 1 || p2.Tasks[0].ID != "task_1" || p2.NextCursor != "" {
		t.Fatalf("page 2: %+v", p2)
	}
	_, err = s.List(context.Background(), ListParams{Agent: AgentRef{"kyber-system", "sol"}, State: StateQueued, Limit: 2, Cursor: p1.NextCursor})
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("filter-mismatched cursor = %v", err)
	}
}
