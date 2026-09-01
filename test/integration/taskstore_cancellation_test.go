//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/taskstore"
)

func newTaskStore(t *testing.T) *taskstore.PostgresStore {
	t.Helper()
	limits := taskstore.DefaultLimits()
	limits.DefaultCancelDeadline = time.Second
	store, err := taskstore.NewPostgresStore(sharedDB, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = sharedDB.Exec(`TRUNCATE agent_tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestTaskStoreCancellationPersistsExactAttemptEvidence(t *testing.T) {
	ctx := context.Background()
	store := newTaskStore(t)
	agent := taskstore.AgentRef{Namespace: "kyber-system", Name: "mat31-integration"}

	queued, err := store.Create(ctx, taskstore.CreateParams{ID: "task_11111111111111111111111111111111", Agent: agent, CreatedBy: "operator", Prompt: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := store.Cancel(ctx, taskstore.CancelParams{Agent: agent, TaskID: queued.Task.ID, RequestedBy: "operator", Reason: "superseded", IdempotencyKey: "queued-key", RequestHash: "queued-hash"})
	if err != nil || !canceled.Applied || canceled.Task.State != taskstore.StateCanceled {
		t.Fatalf("queued cancellation=%+v err=%v", canceled, err)
	}
	replay, err := store.Cancel(ctx, taskstore.CancelParams{Agent: agent, TaskID: queued.Task.ID, RequestedBy: "operator", Reason: "superseded", IdempotencyKey: "queued-key", RequestHash: "queued-hash"})
	if err != nil || !replay.Replay || !replay.Applied {
		t.Fatalf("queued replay=%+v err=%v", replay, err)
	}

	dispatched, err := store.Create(ctx, taskstore.CreateParams{ID: "task_22222222222222222222222222222222", Agent: agent, CreatedBy: "operator", Prompt: "dispatched"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimPending(ctx, "worker-1", time.Minute)
	if err != nil || claim.Task.ID != dispatched.Task.ID {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	attempt := "attempt_33333333333333333333333333333333"
	if err = store.BeginAttempt(ctx, agent, dispatched.Task.ID, "worker-1", attempt); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AcceptReceipt(ctx, agent, taskstore.Receipt{TaskID: dispatched.Task.ID, AttemptID: attempt, Runtime: "codex", SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	canceling, err := store.Cancel(ctx, taskstore.CancelParams{Agent: agent, TaskID: dispatched.Task.ID, RequestedBy: "operator"})
	if err != nil || canceling.Task.State != taskstore.StateCanceling {
		t.Fatalf("dispatched cancellation=%+v err=%v", canceling, err)
	}
	if _, err = store.GetControl(ctx, agent, dispatched.Task.ID, "attempt_44444444444444444444444444444444"); !errors.Is(err, taskstore.ErrInvalidAttempt) {
		t.Fatalf("stale get_control=%v", err)
	}
	control, err := store.GetControl(ctx, agent, dispatched.Task.ID, attempt)
	if err != nil || !control.CancelRequested {
		t.Fatalf("control=%+v err=%v", control, err)
	}
	if _, _, err = store.AcknowledgeCancel(ctx, agent, dispatched.Task.ID, "attempt_44444444444444444444444444444444", "ack_stale", ""); !errors.Is(err, taskstore.ErrInvalidAttempt) {
		t.Fatalf("stale acknowledgment=%v", err)
	}
	final, replayed, err := store.AcknowledgeCancel(ctx, agent, dispatched.Task.ID, attempt, "ack_exact", "stopped")
	if err != nil || replayed || final.State != taskstore.StateCanceled || final.Cancellation.AcknowledgedAt == nil {
		t.Fatalf("acknowledgment=%+v replay=%v err=%v", final, replayed, err)
	}
	restarted, err := taskstore.NewPostgresStore(sharedDB, taskstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Get(ctx, agent, dispatched.Task.ID)
	if err != nil || got.State != taskstore.StateCanceled || got.Cancellation.AckSource != "agent" {
		t.Fatalf("restart read=%+v err=%v", got, err)
	}
}

func TestTaskStoreCancellationDeadlineIsHonestFailure(t *testing.T) {
	ctx := context.Background()
	store := newTaskStore(t)
	agent := taskstore.AgentRef{Namespace: "kyber-system", Name: "mat31-timeout"}
	created, err := store.Create(ctx, taskstore.CreateParams{ID: "task_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Agent: agent, CreatedBy: "operator", Prompt: "timeout"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimPending(ctx, "worker-timeout", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt := "attempt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err = store.BeginAttempt(ctx, agent, claim.Task.ID, "worker-timeout", attempt); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AcceptReceipt(ctx, agent, taskstore.Receipt{TaskID: created.Task.ID, AttemptID: attempt, Runtime: "claude-code", SessionID: "session-timeout"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Cancel(ctx, taskstore.CancelParams{Agent: agent, TaskID: created.Task.ID, RequestedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	if _, err = sharedDB.Exec(`UPDATE agent_tasks SET cancel_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	reconciled, err := store.Reconcile(ctx, 10)
	if err != nil || reconciled.CancelUnconfirmed != 1 {
		t.Fatalf("reconcile=%+v err=%v", reconciled, err)
	}
	got, err := store.Get(ctx, agent, created.Task.ID)
	if err != nil || got.State != taskstore.StateFailed || got.FailureCode != taskstore.FailureCancelUnconfirmed || got.Cancellation == nil {
		t.Fatalf("timeout task=%+v err=%v", got, err)
	}
}
