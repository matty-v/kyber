package messagebuffer_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// kyber#565: the agent finalizer reaps any pending wake messages on delete.

func TestMemoryBuffer_Delete(t *testing.T) {
	ctx := context.Background()
	buf := messagebuffer.NewMemoryBuffer()
	if err := buf.Push(ctx, "dave", messagebuffer.PendingMessage{Source: "telegram", Text: "hi"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := buf.Delete(ctx, "dave"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	msgs, err := buf.Drain(ctx, "dave")
	if err != nil {
		t.Fatalf("Drain after Delete: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Drain after Delete: got %d messages, want 0", len(msgs))
	}
}

func TestMemoryBuffer_DeleteMissing_IsIdempotent(t *testing.T) {
	buf := messagebuffer.NewMemoryBuffer()
	if err := buf.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("Delete on missing: got %v, want nil", err)
	}
}
