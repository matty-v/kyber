//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/taskstore"
)

func TestTaskInteractionPersistsAndQueuesFreshAttempt(t *testing.T) {
	ctx := context.Background()
	store := newTaskStore(t)
	agent := taskstore.AgentRef{Namespace: "kyber-system", Name: "mat32-integration"}
	created, err := store.Create(ctx, taskstore.CreateParams{ID: "task_1234567890abcdef1234567890abcdef", Agent: agent, CreatedBy: "operator", Prompt: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimPending(ctx, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt := "attempt_1234567890abcdef1234567890abcdef"
	if err = store.BeginAttempt(ctx, agent, claim.Task.ID, "worker-1", attempt); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AcceptReceipt(ctx, agent, taskstore.Receipt{TaskID: created.Task.ID, AttemptID: attempt, Runtime: "codex", SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	paused, err := store.RequestInteraction(ctx, taskstore.RequestInteractionParams{Agent: agent, TaskID: created.Task.ID, AttemptID: attempt, InteractionID: "interaction_1234567890abcdef1234567890abcdef", Type: taskstore.InteractionChoice, Question: "region?", Options: []taskstore.InteractionOption{{ID: "us", Label: "US"}, {ID: "eu", Label: "EU"}}})
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != taskstore.StateInputRequired || len(paused.Messages) != 2 {
		t.Fatalf("paused=%+v", paused)
	}
	restarted, err := taskstore.NewPostgresStore(sharedDB, taskstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Get(ctx, agent, created.Task.ID)
	if err != nil || got.Interaction == nil || got.Interaction.Status != taskstore.InteractionPaused {
		t.Fatalf("restart=%+v err=%v", got, err)
	}
	response := json.RawMessage(`"eu"`)
	answered, err := restarted.RespondInteraction(ctx, taskstore.RespondInteractionParams{Agent: agent, TaskID: created.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "operator", IdempotencyKey: "answer-1", RequestHash: "hash", Response: response})
	if err != nil {
		t.Fatal(err)
	}
	if answered.Task.State != taskstore.StateQueued || len(answered.Task.Messages) != 4 {
		t.Fatalf("answered=%+v", answered.Task)
	}
	replay, err := restarted.RespondInteraction(ctx, taskstore.RespondInteractionParams{Agent: agent, TaskID: created.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "operator", IdempotencyKey: "answer-1", RequestHash: "hash", Response: response})
	if err != nil || !replay.Replay {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	next, err := restarted.ClaimPending(ctx, "worker-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next.Task.Interaction == nil || next.Task.Interaction.Status != taskstore.InteractionAnswered {
		t.Fatalf("claim=%+v", next.Task)
	}
	fresh := "attempt_fedcba0987654321fedcba0987654321"
	if err = restarted.BeginAttempt(ctx, agent, next.Task.ID, "worker-2", fresh); err != nil {
		t.Fatal(err)
	}
	after, err := restarted.Get(ctx, agent, next.Task.ID)
	if err != nil || after.Interaction.Status != taskstore.InteractionConsumed {
		t.Fatalf("after=%+v err=%v", after, err)
	}
}

func TestTaskInteractionRejectsStaleAttemptAndExpires(t *testing.T) {
	ctx := context.Background()
	store := newTaskStore(t)
	agent := taskstore.AgentRef{Namespace: "kyber-system", Name: "mat32-races"}
	created, _ := store.Create(ctx, taskstore.CreateParams{ID: "task_abcdefabcdefabcdefabcdefabcdefab", Agent: agent, CreatedBy: "operator", Prompt: "deploy"})
	claim, _ := store.ClaimPending(ctx, "worker", time.Minute)
	attempt := "attempt_abcdefabcdefabcdefabcdefabcdefab"
	_ = store.BeginAttempt(ctx, agent, claim.Task.ID, "worker", attempt)
	_, _, _ = store.AcceptReceipt(ctx, agent, taskstore.Receipt{TaskID: created.Task.ID, AttemptID: attempt, Runtime: "codex", SessionID: "s"})
	_, err := store.RequestInteraction(ctx, taskstore.RequestInteractionParams{Agent: agent, TaskID: created.Task.ID, AttemptID: "attempt_00000000000000000000000000000000", InteractionID: "interaction_abcdefabcdefabcdefabcdefabcdefab", Type: taskstore.InteractionText, Question: "why?"})
	if !errors.Is(err, taskstore.ErrInvalidAttempt) {
		t.Fatalf("stale err=%v", err)
	}
	paused, err := store.RequestInteraction(ctx, taskstore.RequestInteractionParams{Agent: agent, TaskID: created.Task.ID, AttemptID: attempt, InteractionID: "interaction_abcdefabcdefabcdefabcdefabcdefab", Type: taskstore.InteractionText, Question: "why?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sharedDB.Exec(`UPDATE agent_task_interactions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, paused.Interaction.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RespondInteraction(ctx, taskstore.RespondInteractionParams{Agent: agent, TaskID: created.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "operator", Response: json.RawMessage(`"late"`)}); !errors.Is(err, taskstore.ErrInteractionExpired) {
		t.Fatalf("expiry err=%v", err)
	}
}

func TestPostgresJSONInteractionEnforcesPersistedSchema(t *testing.T) {
	ctx := context.Background()
	store := newTaskStore(t)
	agent := taskstore.AgentRef{Namespace: "kyber-system", Name: "mat32-json-schema"}
	created, err := store.Create(ctx, taskstore.CreateParams{ID: "task_99999999999999999999999999999999", Agent: agent, CreatedBy: "operator", Prompt: "configure"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimPending(ctx, "schema-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt := "attempt_99999999999999999999999999999999"
	if err = store.BeginAttempt(ctx, agent, claim.Task.ID, "schema-worker", attempt); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.AcceptReceipt(ctx, agent, taskstore.Receipt{TaskID: created.Task.ID, AttemptID: attempt, Runtime: "codex", SessionID: "schema-session"}); err != nil {
		t.Fatal(err)
	}
	paused, err := store.RequestInteraction(ctx, taskstore.RequestInteractionParams{
		Agent: agent, TaskID: created.Task.ID, AttemptID: attempt,
		InteractionID: "interaction_99999999999999999999999999999999",
		Type:          taskstore.InteractionJSON, Question: "configuration?",
		Schema: json.RawMessage(`{"type":"object","required":["region"],"properties":{"region":{"type":"string"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.RespondInteraction(ctx, taskstore.RespondInteractionParams{Agent: agent, TaskID: created.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "operator", Response: json.RawMessage(`{"region":42}`)})
	if !errors.Is(err, taskstore.ErrInvalid) {
		t.Fatalf("schema-invalid response err=%v", err)
	}
}
