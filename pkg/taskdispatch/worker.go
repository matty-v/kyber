// Package taskdispatch wakes durable PostgreSQL dispatch intents and feeds
// them through Kyber's existing serialized per-agent delivery boundary.
package taskdispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/matty-v/kyber/pkg/inbound"
	"github.com/matty-v/kyber/pkg/taskstore"
)

type Worker struct {
	Store         taskstore.DispatchStore
	Queue         *inbound.Queue
	Owner         string
	PollInterval  time.Duration
	LeaseDuration time.Duration
}

func (w *Worker) Run(ctx context.Context) {
	poll := w.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	lease := w.LeaseDuration
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		w.drain(ctx, lease)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) drain(ctx context.Context, lease time.Duration) {
	for i := 0; i < 32; i++ {
		claim, err := w.Store.ClaimPending(ctx, w.Owner, lease)
		if errors.Is(err, taskstore.ErrNoDispatch) {
			return
		}
		if err != nil {
			slog.Error("tasks: claim failed", "error", err)
			return
		}
		w.enqueue(ctx, claim, lease)
	}
}

func (w *Worker) enqueue(ctx context.Context, claim *taskstore.DispatchClaim, lease time.Duration) {
	a := taskstore.AgentRef{Namespace: claim.Task.AgentNamespace, Name: claim.Task.AgentName}
	attempt, err := newAttemptID()
	if err != nil {
		_ = w.Store.ReleaseLease(ctx, a, claim.Task.ID, w.Owner, time.Second)
		return
	}
	var started atomic.Bool
	renewCtx, cancelRenew := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if err := w.Store.RenewLease(renewCtx, a, claim.Task.ID, w.Owner, lease); err != nil {
					slog.Warn("tasks: lease renewal failed", "task_id", claim.Task.ID, "error", err)
					return
				}
			}
		}
	}()
	job := inbound.Job{Agent: claim.Task.AgentName, Binding: "platform-task", RequestID: claim.Task.ID, Envelope: envelope(claim.Task, attempt), EnqueuedAt: time.Now().UTC(), Kind: inbound.JobKindTask, DeliverBefore: claim.Task.DeadlineAt,
		BeforeDelivery: func(callCtx context.Context) error {
			cancelRenew()
			err := w.Store.BeginAttempt(callCtx, a, claim.Task.ID, w.Owner, attempt)
			if err == nil {
				started.Store(true)
			}
			return err
		},
		OnDelivery: func(callCtx context.Context, outcome inbound.DeliveryOutcome) {
			cancelRenew()
			switch outcome {
			case inbound.DeliveryDispatched:
				if err := w.Store.MarkReceiptPending(callCtx, a, claim.Task.ID, w.Owner, attempt); err != nil {
					slog.Error("tasks: recording receipt wait failed", "task_id", claim.Task.ID, "error", err)
				}
			case inbound.DeliveryAgentUnavailable:
				_ = w.Store.FailDelivery(callCtx, a, claim.Task.ID, w.Owner, claim.Task.Version, taskstore.FailureAgentUnavailable)
			case inbound.DeliveryFailed:
				if started.Load() {
					if err := w.Store.FailDelivery(callCtx, a, claim.Task.ID, w.Owner, claim.Task.Version, taskstore.FailureDeliveryUnknown); err != nil {
						slog.Warn("tasks: closing ambiguous delivery failed; reconciler will retry", "task_id", claim.Task.ID, "error", err)
					}
				} else {
					_ = w.Store.ReleaseLease(callCtx, a, claim.Task.ID, w.Owner, time.Second)
				}
			}
		}}
	if err := w.Queue.Enqueue(job); err != nil {
		cancelRenew()
		_ = w.Store.ReleaseLease(ctx, a, claim.Task.ID, w.Owner, time.Second)
	}
}

func envelope(t *taskstore.Task, attempt string) string {
	prompt, _ := json.Marshal(t.Prompt)
	continuation := ""
	if t.Interaction != nil && t.Interaction.Status == taskstore.InteractionAnswered {
		question, _ := json.Marshal(t.Interaction.Question)
		response, _ := json.Marshal(json.RawMessage(t.Interaction.Response))
		continuation = fmt.Sprintf("  continuation:\n    interaction_id: %s\n    type: %s\n    question_json: %s\n    response_json: %s\n", t.Interaction.ID, t.Interaction.Type, question, response)
	}
	return fmt.Sprintf("[kyber-task:%s] attempt=%s\nagent: %s\ntask:\n  id: %s\n  attempt_id: %s\n  deadline_at: %s\n  prompt_json: %s\n%saction:\n  Handle only this task. Check kyber-request-reply.get_control before each material phase and after long-running tools. If cancellation is requested, stop future task work and call ack_cancel with this exact task and attempt; cancellation does not roll back prior external effects. If input is required, call request_input or request_authorization and end the turn immediately. Otherwise finish by calling complete with this task id, attempt id, and bounded response.\n", t.ID, attempt, t.AgentName, t.ID, attempt, t.DeadlineAt.UTC().Format(time.RFC3339Nano), prompt, continuation)
}
func newAttemptID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "attempt_" + hex.EncodeToString(b[:]), nil
}
