package messagebuffer_test

import (
	"context"
	"sync"
	"testing"

	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// compile-time assertion: MemoryBuffer satisfies MessageBuffer.
var _ messagebuffer.MessageBuffer = (*messagebuffer.MemoryBuffer)(nil)

func TestMemoryBuffer_PushDrain_ReturnsInOrder(t *testing.T) {
	ctx := context.Background()
	buf := messagebuffer.NewMemoryBuffer()

	msg1 := messagebuffer.PendingMessage{Source: "telegram", From: "u1", Text: "hello", Timestamp: "2026-01-01T00:00:00Z"}
	msg2 := messagebuffer.PendingMessage{Source: "telegram", From: "u1", Text: "world", Timestamp: "2026-01-01T00:00:01Z"}

	if err := buf.Push(ctx, "dave", msg1); err != nil {
		t.Fatalf("Push msg1: %v", err)
	}
	if err := buf.Push(ctx, "dave", msg2); err != nil {
		t.Fatalf("Push msg2: %v", err)
	}

	msgs, err := buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Drain len: got %d, want 2", len(msgs))
	}
	if msgs[0].Text != "hello" {
		t.Errorf("msgs[0].Text: got %q, want %q", msgs[0].Text, "hello")
	}
	if msgs[1].Text != "world" {
		t.Errorf("msgs[1].Text: got %q, want %q", msgs[1].Text, "world")
	}
}

func TestMemoryBuffer_DrainEmpty_ReturnsEmptySlice(t *testing.T) {
	ctx := context.Background()
	buf := messagebuffer.NewMemoryBuffer()

	msgs, err := buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("Drain on empty: %v", err)
	}
	if msgs == nil {
		t.Error("Drain on empty returned nil, want empty slice")
	}
	if len(msgs) != 0 {
		t.Errorf("Drain on empty: got len %d, want 0", len(msgs))
	}
}

func TestMemoryBuffer_DrainTwice_SecondDrainEmpty(t *testing.T) {
	ctx := context.Background()
	buf := messagebuffer.NewMemoryBuffer()

	msg := messagebuffer.PendingMessage{Source: "telegram", From: "u1", Text: "hi", Timestamp: "2026-01-01T00:00:00Z"}
	if err := buf.Push(ctx, "dave", msg); err != nil {
		t.Fatalf("Push: %v", err)
	}

	first, err := buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first Drain len: got %d, want 1", len(first))
	}

	second, err := buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second Drain len: got %d, want 0", len(second))
	}
}

func TestMemoryBuffer_IsolatedPerAgent(t *testing.T) {
	ctx := context.Background()
	buf := messagebuffer.NewMemoryBuffer()

	msgDave := messagebuffer.PendingMessage{Source: "telegram", From: "u1", Text: "for dave", Timestamp: "2026-01-01T00:00:00Z"}
	msgChewie := messagebuffer.PendingMessage{Source: "telegram", From: "u2", Text: "for chewie", Timestamp: "2026-01-01T00:00:01Z"}

	if err := buf.Push(ctx, "dave", msgDave); err != nil {
		t.Fatalf("Push dave: %v", err)
	}
	if err := buf.Push(ctx, "chewie", msgChewie); err != nil {
		t.Fatalf("Push chewie: %v", err)
	}

	daveMsgs, err := buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("Drain dave: %v", err)
	}
	if len(daveMsgs) != 1 || daveMsgs[0].Text != "for dave" {
		t.Errorf("dave msgs: got %v", daveMsgs)
	}

	chewieMsgs, err := buf.Drain(ctx, "chewie")
	if err != nil {
		t.Fatalf("Drain chewie: %v", err)
	}
	if len(chewieMsgs) != 1 || chewieMsgs[0].Text != "for chewie" {
		t.Errorf("chewie msgs: got %v", chewieMsgs)
	}
}

func TestMemoryBuffer_ConcurrentPush_Safe(t *testing.T) {
	ctx := context.Background()
	buf := messagebuffer.NewMemoryBuffer()

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			msg := messagebuffer.PendingMessage{Source: "telegram", From: "u", Text: "msg", Timestamp: "2026-01-01T00:00:00Z"}
			if err := buf.Push(ctx, "dave", msg); err != nil {
				t.Errorf("concurrent Push: %v", err)
			}
		}()
	}
	wg.Wait()

	msgs, err := buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("Drain after concurrent Push: %v", err)
	}
	if len(msgs) != goroutines {
		t.Errorf("Drain len: got %d, want %d", len(msgs), goroutines)
	}
}
