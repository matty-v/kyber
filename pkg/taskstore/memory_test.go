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

func TestMemoryCancellationQueuedAndDispatched(t *testing.T) {
	s := testStore(t)
	a := AgentRef{"kyber-system", "sol"}
	queued := createTask(t, s, "task_cancel_queued", "", "").Task
	result, err := s.Cancel(context.Background(), CancelParams{Agent: a, TaskID: queued.ID, RequestedBy: "operator", Reason: "superseded", IdempotencyKey: "cancel-1", RequestHash: "hash-1"})
	if err != nil || !result.Applied || result.Task.State != StateCanceled || result.Task.Cancellation == nil || result.Task.Cancellation.Scope != "future_task_work" {
		t.Fatalf("queued cancel=%+v err=%v", result, err)
	}
	replay, err := s.Cancel(context.Background(), CancelParams{Agent: a, TaskID: queued.ID, RequestedBy: "operator", Reason: "superseded", IdempotencyKey: "cancel-1", RequestHash: "hash-1"})
	if err != nil || !replay.Replay || !replay.Applied {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	_, err = s.Cancel(context.Background(), CancelParams{Agent: a, TaskID: queued.ID, RequestedBy: "operator", Reason: "different", IdempotencyKey: "cancel-1", RequestHash: "hash-2"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict=%v", err)
	}

	dispatched := createTask(t, s, "task_cancel_dispatched", "", "").Task
	if err = s.MarkDispatched(context.Background(), a, dispatched.ID, dispatched.Version); err != nil {
		t.Fatal(err)
	}
	result, err = s.Cancel(context.Background(), CancelParams{Agent: a, TaskID: dispatched.ID, RequestedBy: "operator"})
	if err != nil || result.Task.State != StateCanceling {
		t.Fatalf("dispatched cancel=%+v err=%v", result, err)
	}
	control, err := s.GetControl(context.Background(), a, dispatched.ID, "attempt_current")
	if err != nil || !control.CancelRequested {
		t.Fatalf("control=%+v err=%v", control, err)
	}
	ack, replayed, err := s.AcknowledgeCancel(context.Background(), a, dispatched.ID, "attempt_current", "ack_1", "stopped")
	if err != nil || replayed || ack.State != StateCanceled || ack.Cancellation.AcknowledgedAt == nil {
		t.Fatalf("ack=%+v replay=%v err=%v", ack, replayed, err)
	}
}

func TestMemoryCancellationTerminalRaces(t *testing.T) {
	s := testStore(t)
	a := AgentRef{"kyber-system", "sol"}
	completionWins := createTask(t, s, "task_completion_wins", "", "").Task
	if err := s.MarkDispatched(context.Background(), a, completionWins.ID, 1); err != nil {
		t.Fatal(err)
	}
	cancel, err := s.Cancel(context.Background(), CancelParams{Agent: a, TaskID: completionWins.ID, RequestedBy: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(context.Background(), a, completionWins.ID, cancel.Task.Version, "finished before observing cancel"); err != nil {
		t.Fatalf("completion should honestly win: %v", err)
	}
	if _, _, err = s.AcknowledgeCancel(context.Background(), a, completionWins.ID, "attempt_current", "ack_after_completion", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("late acknowledgment=%v", err)
	}

	ackWins := createTask(t, s, "task_ack_wins", "", "").Task
	if err = s.MarkDispatched(context.Background(), a, ackWins.ID, 1); err != nil {
		t.Fatal(err)
	}
	cancel, err = s.Cancel(context.Background(), CancelParams{Agent: a, TaskID: ackWins.ID, RequestedBy: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	acknowledged, _, err := s.AcknowledgeCancel(context.Background(), a, ackWins.ID, "attempt_current", "ack_wins", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(context.Background(), a, ackWins.ID, acknowledged.Version, "too late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("late completion=%v", err)
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

func TestMemoryListFiltersBeforePaginationAndReportsTotal(t *testing.T) {
	s := testStore(t)
	a := AgentRef{"kyber-system", "sol"}
	for _, tc := range []struct {
		id          string
		correlation string
	}{
		{id: "task_1", correlation: "context-a"},
		{id: "task_2", correlation: "context-b"},
		{id: "task_3", correlation: "context-a"},
	} {
		_, err := s.Create(context.Background(), CreateParams{
			ID: tc.id, Agent: a, CreatedBy: "operator", Prompt: "ship it", Correlation: tc.correlation,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.List(context.Background(), ListParams{Agent: a, Correlation: "context-a", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Tasks) != 1 || page.Tasks[0].ID != "task_3" || page.NextCursor == "" {
		t.Fatalf("filtered page: %+v", page)
	}
	next, err := s.List(context.Background(), ListParams{Agent: a, Correlation: "context-a", Limit: 1, Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if next.Total != 2 || len(next.Tasks) != 1 || next.Tasks[0].ID != "task_1" || next.NextCursor != "" {
		t.Fatalf("filtered next page: %+v", next)
	}
}

func TestMemoryListCursorBindsA2AFilters(t *testing.T) {
	s := testStore(t)
	a := AgentRef{"kyber-system", "sol"}
	for _, id := range []string{"task_1", "task_2"} {
		createTask(t, s, id, "", "")
	}
	updatedAfter := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	page, err := s.List(context.Background(), ListParams{
		Agent: a, States: []State{StateQueued, StateDispatched}, UpdatedAfter: updatedAfter, Limit: 1,
	})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	_, err = s.List(context.Background(), ListParams{
		Agent: a, States: []State{StateQueued, StateDispatched}, UpdatedAfter: updatedAfter.Add(time.Hour), Limit: 1, Cursor: page.NextCursor,
	})
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("updated-after-mismatched cursor = %v", err)
	}
	_, err = s.List(context.Background(), ListParams{
		Agent: a, States: []State{StateQueued, StateDispatched}, UpdatedAfter: updatedAfter, Correlation: "different", Limit: 1, Cursor: page.NextCursor,
	})
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("correlation-mismatched cursor = %v", err)
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
