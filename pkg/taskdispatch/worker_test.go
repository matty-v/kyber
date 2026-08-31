package taskdispatch

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/inbound"
	"github.com/matty-v/kyber/pkg/taskstore"
)

type fakeStore struct {
	taskstore.DispatchStore
	mu             sync.Mutex
	claim          *taskstore.DispatchClaim
	began, pending string
}

func (f *fakeStore) ClaimPending(context.Context, string, time.Duration) (*taskstore.DispatchClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claim == nil {
		return nil, taskstore.ErrNoDispatch
	}
	c := f.claim
	f.claim = nil
	return c, nil
}
func (f *fakeStore) BeginAttempt(_ context.Context, _ taskstore.AgentRef, id, owner, attempt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.began = id + ":" + owner + ":" + attempt
	return nil
}
func (f *fakeStore) MarkReceiptPending(_ context.Context, _ taskstore.AgentRef, id, owner, attempt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = id + ":" + owner + ":" + attempt
	return nil
}
func (f *fakeStore) RenewLease(context.Context, taskstore.AgentRef, string, string, time.Duration) error {
	return nil
}
func (f *fakeStore) ReleaseLease(context.Context, taskstore.AgentRef, string, string, time.Duration) error {
	return nil
}

func TestWorkerPersistsAttemptBeforeDelivery(t *testing.T) {
	task := &taskstore.Task{ID: "task_11111111111111111111111111111111", AgentNamespace: "kyber-system", AgentName: "sol", Prompt: "work", State: taskstore.StateQueued, Version: 1, DeadlineAt: time.Now().Add(time.Hour)}
	store := &fakeStore{claim: &taskstore.DispatchClaim{Task: task}}
	delivered := make(chan inbound.Job, 1)
	queue := inbound.NewQueue(func(ctx context.Context, job inbound.Job) {
		if err := job.BeforeDelivery(ctx); err != nil {
			t.Error(err)
			return
		}
		delivered <- job
		job.OnDelivery(ctx, inbound.DeliveryDispatched)
	})
	defer queue.Stop()
	worker := &Worker{Store: store, Queue: queue, Owner: "cp-1"}
	worker.drain(context.Background(), time.Minute)
	select {
	case job := <-delivered:
		if !strings.Contains(job.Envelope, "[kyber-task:"+task.ID+"] attempt=attempt_") {
			t.Fatalf("envelope=%s", job.Envelope)
		}
	case <-time.After(time.Second):
		t.Fatal("not delivered")
	}
	deadline := time.Now().Add(time.Second)
	for {
		store.mu.Lock()
		began, pending := store.began, store.pending
		store.mu.Unlock()
		if strings.Contains(began, task.ID+":cp-1:attempt_") && strings.Contains(pending, task.ID+":cp-1:attempt_") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("began=%q pending=%q", began, pending)
		}
		time.Sleep(time.Millisecond)
	}
}
