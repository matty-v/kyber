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

func TestMemoryProgressIdempotencyAndConflict(t *testing.T) {
	s := testStore(t)
	task := createTask(t, s, "task_progress", "", "").Task
	a := AgentRef{"kyber-system", "sol"}
	if err := s.MarkDispatched(context.Background(), a, task.ID, task.Version); err != nil {
		t.Fatal(err)
	}
	percent := 40
	p, replay, err := s.ReportProgress(context.Background(), a, task.ID, "attempt_1", ProgressUpdate{UpdateID: "update_1", Message: "working", Percent: &percent})
	if err != nil || replay || p.Message != "working" {
		t.Fatalf("first: %+v %v %v", p, replay, err)
	}
	_, replay, err = s.ReportProgress(context.Background(), a, task.ID, "attempt_1", ProgressUpdate{UpdateID: "update_1", Message: "working", Percent: &percent})
	if err != nil || !replay {
		t.Fatalf("replay: %v %v", replay, err)
	}
	_, _, err = s.ReportProgress(context.Background(), a, task.ID, "attempt_1", ProgressUpdate{UpdateID: "update_1", Message: "different"})
	if !errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("conflict=%v", err)
	}
}

func TestMemoryImmutableResultAndLegacyResponse(t *testing.T) {
	s := testStore(t)
	task := createTask(t, s, "task_result", "", "").Task
	a := AgentRef{"kyber-system", "sol"}
	if err := s.MarkDispatched(context.Background(), a, task.ID, 1); err != nil {
		t.Fatal(err)
	}
	r := Result{ID: "result_1", Name: "report", Parts: []ResultPart{{ID: "part_1", Kind: PartText, Text: "hello"}}}
	saved, replay, err := s.PublishResult(context.Background(), a, task.ID, "attempt_1", r)
	if err != nil || replay || saved.ContentDigest == "" {
		t.Fatalf("publish: %+v %v %v", saved, replay, err)
	}
	_, replay, err = s.PublishResult(context.Background(), a, task.ID, "attempt_1", r)
	if err != nil || !replay {
		t.Fatalf("replay: %v %v", replay, err)
	}
	r.Parts[0].Text = "changed"
	_, _, err = s.PublishResult(context.Background(), a, task.ID, "attempt_1", r)
	if !errors.Is(err, ErrResultConflict) {
		t.Fatalf("conflict=%v", err)
	}

	legacy := createTask(t, s, "task_legacy", "", "").Task
	if err = s.MarkDispatched(context.Background(), a, legacy.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(context.Background(), a, legacy.ID, 2, "done"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), a, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Response != "done" || len(got.Results) != 1 || got.Results[0].Name != "response" || got.Results[0].Parts[0].Text != "done" {
		t.Fatalf("legacy=%+v", got)
	}
}

func TestMemoryFileResultReplayIgnoresPrivateObjectIdentity(t *testing.T) {
	s := testStore(t)
	task := createTask(t, s, "task_file", "", "").Task
	a := AgentRef{"kyber-system", "sol"}
	if err := s.MarkDispatched(context.Background(), a, task.ID, 1); err != nil {
		t.Fatal(err)
	}
	result := Result{ID: "result_file", Name: "report", Parts: []ResultPart{{ID: "part_file", Kind: PartFile, File: &FileMetadata{ObjectID: "temporary-1", Filename: "report.pdf", MediaType: "application/pdf", SizeBytes: 4, SHA256: "abcd", ScanStatus: "not_configured"}}}}
	stored, replay, err := s.PublishResult(context.Background(), a, task.ID, "attempt_1", result)
	if err != nil || replay || stored.Parts[0].File.ObjectID != "temporary-1" {
		t.Fatalf("first=%+v replay=%v err=%v", stored, replay, err)
	}
	result.Parts[0].File.ObjectID = "temporary-2"
	stored, replay, err = s.PublishResult(context.Background(), a, task.ID, "attempt_1", result)
	if err != nil || !replay || stored.Parts[0].File.ObjectID != "temporary-1" {
		t.Fatalf("replay=%+v replay=%v err=%v", stored, replay, err)
	}
}
