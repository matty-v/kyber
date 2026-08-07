package inbound

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueEnqueueDepthAndOverflow(t *testing.T) {
	// Block the worker on the first job so the bounded channel buffer fills
	// up. Spec says "queue holds 5 jobs"; we accept at least QueueDepth
	// successes (the worker may also be holding one in flight) and require
	// the next attempt past that to overflow.
	gate := make(chan struct{})
	q := NewQueue(func(_ context.Context, _ Job) {
		<-gate
	})
	defer func() {
		close(gate)
		q.Stop()
	}()

	successes := 0
	const tryN = QueueDepth + 5
	var firstFail error
	for i := 0; i < tryN; i++ {
		err := q.Enqueue(Job{Agent: "ag", Binding: "b", RequestID: "r", Envelope: "x"})
		if err == nil {
			successes++
			continue
		}
		firstFail = err
		break
	}
	if successes < QueueDepth {
		t.Fatalf("expected at least %d successful enqueues, got %d (first fail: %v)", QueueDepth, successes, firstFail)
	}
	if successes > QueueDepth+1 {
		t.Fatalf("expected at most %d successful enqueues (buffer + in-flight), got %d", QueueDepth+1, successes)
	}
	if !errors.Is(firstFail, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull on overflow, got %v", firstFail)
	}
}

func TestQueueFIFOOrder(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	done := make(chan struct{})
	wantN := 5

	q := NewQueue(func(_ context.Context, job Job) {
		mu.Lock()
		got = append(got, job.RequestID)
		if len(got) == wantN {
			close(done)
		}
		mu.Unlock()
	})
	defer q.Stop()

	for i := 0; i < wantN; i++ {
		id := string(rune('A' + i))
		if err := q.Enqueue(Job{Agent: "ag", RequestID: id}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler to drain")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"A", "B", "C", "D", "E"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FIFO violated: got %v want %v", got, want)
		}
	}
}

func TestQueuePerAgentIsolation(t *testing.T) {
	gateA := make(chan struct{})
	var aSeen, bSeen int32
	q := NewQueue(func(_ context.Context, job Job) {
		switch job.Agent {
		case "a":
			atomic.AddInt32(&aSeen, 1)
			<-gateA
		case "b":
			atomic.AddInt32(&bSeen, 1)
		}
	})
	defer func() {
		close(gateA)
		q.Stop()
	}()

	// Block agent a's worker by enqueueing once and letting it stall on gateA.
	if err := q.Enqueue(Job{Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	// Agent b should still drain freely.
	if err := q.Enqueue(Job{Agent: "b"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(time.Second)
	for atomic.LoadInt32(&bSeen) == 0 {
		select {
		case <-deadline:
			t.Fatal("agent b's worker did not run despite agent a being blocked")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestQueueStopIdempotent(t *testing.T) {
	q := NewQueue(func(_ context.Context, _ Job) {})
	if err := q.Enqueue(Job{Agent: "x"}); err != nil {
		t.Fatal(err)
	}
	q.Stop()
	q.Stop() // must not panic.

	// Post-stop enqueues should be rejected.
	if err := q.Enqueue(Job{Agent: "x"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("post-Stop Enqueue should return ErrQueueFull, got %v", err)
	}
}

func TestQueueStopDrainsInFlight(t *testing.T) {
	var seen int32
	q := NewQueue(func(_ context.Context, _ Job) {
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&seen, 1)
	})

	for i := 0; i < 3; i++ {
		if err := q.Enqueue(Job{Agent: "a"}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	q.Stop()
	if got := atomic.LoadInt32(&seen); got != 3 {
		t.Fatalf("Stop should wait for all in-flight jobs; seen=%d want 3", got)
	}
}
