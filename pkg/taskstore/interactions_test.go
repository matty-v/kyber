package taskstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMemoryInteractionPauseResumeAndReplay(t *testing.T) {
	s, err := NewMemoryStore(DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := AgentRef{Namespace: "ns", Name: "agent"}
	created, err := s.Create(ctx, CreateParams{ID: "task_11111111111111111111111111111111", Agent: a, CreatedBy: "alice", Prompt: "deploy", DeadlineAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkDispatched(ctx, a, created.Task.ID, created.Task.Version); err != nil {
		t.Fatal(err)
	}
	paused, err := s.RequestInteraction(ctx, RequestInteractionParams{Agent: a, TaskID: created.Task.ID, AttemptID: "attempt_22222222222222222222222222222222", InteractionID: "interaction_33333333333333333333333333333333", Type: InteractionChoice, Question: "region?", Options: []InteractionOption{{ID: "us", Label: "US"}, {ID: "eu", Label: "EU"}}})
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != StateInputRequired || paused.Interaction.Status != InteractionPaused || len(paused.Messages) != 2 {
		t.Fatalf("paused=%+v", paused)
	}
	_, err = s.RequestInteraction(ctx, RequestInteractionParams{Agent: a, TaskID: created.Task.ID, AttemptID: "attempt_22222222222222222222222222222222", InteractionID: "interaction_44444444444444444444444444444444", Type: InteractionText, Question: "other?"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second interaction err=%v", err)
	}
	bad := json.RawMessage(`"apac"`)
	_, err = s.RespondInteraction(ctx, RespondInteractionParams{Agent: a, TaskID: created.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "alice", Response: bad})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad choice err=%v", err)
	}
	response := json.RawMessage(`"eu"`)
	res, err := s.RespondInteraction(ctx, RespondInteractionParams{Agent: a, TaskID: created.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "alice", IdempotencyKey: "answer-1", RequestHash: "hash", Response: response})
	if err != nil {
		t.Fatal(err)
	}
	if res.Task.State != StateQueued || res.Task.Interaction.Status != InteractionAnswered || len(res.Task.Messages) != 4 {
		t.Fatalf("resumed=%+v", res.Task)
	}
	replay, err := s.RespondInteraction(ctx, RespondInteractionParams{Agent: a, TaskID: created.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "alice", IdempotencyKey: "answer-1", RequestHash: "hash", Response: response})
	if err != nil || !replay.Replay {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestMemoryAuthorizationRequiresOpaqueReferenceAndOwner(t *testing.T) {
	s, _ := NewMemoryStore(DefaultLimits())
	ctx := context.Background()
	a := AgentRef{Namespace: "ns", Name: "agent"}
	cr, _ := s.Create(ctx, CreateParams{ID: "task_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Agent: a, CreatedBy: "alice", Prompt: "ship", DeadlineAt: time.Now().Add(time.Hour)})
	_ = s.MarkDispatched(ctx, a, cr.Task.ID, cr.Task.Version)
	paused, err := s.RequestInteraction(ctx, RequestInteractionParams{Agent: a, TaskID: cr.Task.ID, AttemptID: "attempt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", InteractionID: "interaction_cccccccccccccccccccccccccccccccc", Type: InteractionAuthorization, Question: "authorize github", AuthorizationFlow: "github-app"})
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != StateAuthRequired {
		t.Fatal(paused.State)
	}
	flowID := paused.Interaction.AuthorizationFlow
	_, err = s.RespondInteraction(ctx, RespondInteractionParams{Agent: a, TaskID: cr.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "bob", Response: json.RawMessage(`{"authorizationFlowId":"` + flowID + `"}`)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner err=%v", err)
	}
	_, err = s.RespondInteraction(ctx, RespondInteractionParams{Agent: a, TaskID: cr.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "alice", Response: json.RawMessage(`{"token":"secret"}`)})
	if !errors.Is(err, ErrAuthorizationFlow) {
		t.Fatalf("credential err=%v", err)
	}
	if err = s.CompleteAuthorizationFlow(ctx, CompleteAuthorizationFlowParams{FlowID: flowID, TaskID: cr.Task.ID, InteractionID: paused.Interaction.ID, CreatedBy: "alice", ConnectionReference: "connection_github_1"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.RespondInteraction(ctx, RespondInteractionParams{Agent: a, TaskID: cr.Task.ID, InteractionID: paused.Interaction.ID, RespondedBy: "alice", Response: json.RawMessage(`{"authorizationFlowId":"` + flowID + `"}`)})
	if err != nil || res.Task.State != StateQueued || string(res.Task.Interaction.Response) != `{"reference":"connection_github_1"}` {
		t.Fatalf("authorization result=%+v err=%v", res, err)
	}
}

func TestMemoryCancelPausedTaskIsImmediate(t *testing.T) {
	s, _ := NewMemoryStore(DefaultLimits())
	ctx := context.Background()
	a := AgentRef{Namespace: "ns", Name: "agent"}
	cr, _ := s.Create(ctx, CreateParams{ID: "task_dddddddddddddddddddddddddddddddd", Agent: a, CreatedBy: "alice", Prompt: "ship", DeadlineAt: time.Now().Add(time.Hour)})
	_ = s.MarkDispatched(ctx, a, cr.Task.ID, cr.Task.Version)
	_, _ = s.RequestInteraction(ctx, RequestInteractionParams{Agent: a, TaskID: cr.Task.ID, AttemptID: "attempt_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", InteractionID: "interaction_ffffffffffffffffffffffffffffffff", Type: InteractionConfirm, Question: "continue?"})
	res, err := s.Cancel(ctx, CancelParams{Agent: a, TaskID: cr.Task.ID, RequestedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Task.State != StateCanceled {
		t.Fatalf("state=%s", res.Task.State)
	}
}
