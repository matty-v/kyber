//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// cleanRedisKey deletes a Redis key so tests are isolated.
func cleanRedisKey(t *testing.T, key string) {
	t.Helper()
	if err := sharedRDB.Del(context.Background(), key).Err(); err != nil {
		t.Fatalf("cleaning redis key %q: %v", key, err)
	}
}

// newRedisBuffer returns a RedisBuffer backed by the shared Redis client.
func newRedisBuffer() *messagebuffer.RedisBuffer {
	return messagebuffer.NewRedisBuffer(sharedRDB)
}

// TestRedisBuffer_PushAndDrain verifies the full push/drain roundtrip.
func TestRedisBuffer_PushAndDrain(t *testing.T) {
	cleanRedisKey(t, "wake:test-agent")
	buf := newRedisBuffer()
	ctx := context.Background()

	msgs := []messagebuffer.PendingMessage{
		{Source: "telegram", From: "@matt", Text: "hello", Timestamp: "2026-04-10T00:00:00Z"},
		{Source: "telegram", From: "@matt", Text: "world", Timestamp: "2026-04-10T00:00:01Z"},
	}

	for _, msg := range msgs {
		if err := buf.Push(ctx, "test-agent", msg); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}

	got, err := buf.Drain(ctx, "test-agent")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("Drain returned %d messages, want 2", len(got))
	}
	if got[0].Text != "hello" {
		t.Errorf("got[0].Text = %q, want %q", got[0].Text, "hello")
	}
	if got[1].Text != "world" {
		t.Errorf("got[1].Text = %q, want %q", got[1].Text, "world")
	}
}

// TestRedisBuffer_DrainEmptyReturnsEmpty verifies Drain on an empty key returns a non-nil empty slice.
func TestRedisBuffer_DrainEmptyReturnsEmpty(t *testing.T) {
	cleanRedisKey(t, "wake:no-messages-agent")
	buf := newRedisBuffer()
	ctx := context.Background()

	got, err := buf.Drain(ctx, "no-messages-agent")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got == nil {
		t.Error("Drain returned nil, expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("Drain returned %d messages, want 0", len(got))
	}
}

// TestRedisBuffer_DrainIsAtomic verifies Drain empties the queue — a second Drain returns empty.
func TestRedisBuffer_DrainIsAtomic(t *testing.T) {
	cleanRedisKey(t, "wake:atomic-agent")
	buf := newRedisBuffer()
	ctx := context.Background()

	msg := messagebuffer.PendingMessage{Source: "telegram", From: "@matt", Text: "one-shot", Timestamp: "2026-04-10T00:00:00Z"}
	if err := buf.Push(ctx, "atomic-agent", msg); err != nil {
		t.Fatalf("Push: %v", err)
	}

	first, err := buf.Drain(ctx, "atomic-agent")
	if err != nil {
		t.Fatalf("Drain(first): %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first Drain: got %d messages, want 1", len(first))
	}

	// Second Drain: queue must be empty after first.
	second, err := buf.Drain(ctx, "atomic-agent")
	if err != nil {
		t.Fatalf("Drain(second): %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second Drain: got %d messages, want 0 (drain should be atomic)", len(second))
	}
}

// TestRedisBuffer_TTLIsSet verifies that after Push the key has a positive TTL (~5 minutes).
func TestRedisBuffer_TTLIsSet(t *testing.T) {
	cleanRedisKey(t, "wake:ttl-agent")
	buf := newRedisBuffer()
	ctx := context.Background()

	msg := messagebuffer.PendingMessage{Source: "telegram", From: "@matt", Text: "ttl-check", Timestamp: "2026-04-10T00:00:00Z"}
	if err := buf.Push(ctx, "ttl-agent", msg); err != nil {
		t.Fatalf("Push: %v", err)
	}

	ttl, err := sharedRDB.TTL(ctx, "wake:ttl-agent").Result()
	if err != nil {
		t.Fatalf("TTL command: %v", err)
	}
	// TTL should be set and be close to 5 minutes (allow 30s of drift for test latency).
	if ttl <= 0 {
		t.Errorf("TTL not set or key already expired: ttl=%v", ttl)
	}
	const expectedTTL = 5 * time.Minute
	const tolerance = 30 * time.Second
	if ttl > expectedTTL+tolerance {
		t.Errorf("TTL too long: got %v, expected ~%v", ttl, expectedTTL)
	}
	// Must be at least 4.5 minutes (spec is 5-minute TTL, pushed just now).
	if ttl < expectedTTL-tolerance {
		t.Errorf("TTL too short: got %v, expected ~%v", ttl, expectedTTL)
	}
}

// TestRedisBuffer_TTLRefreshedOnMultiplePush verifies TTL is refreshed on each Push.
func TestRedisBuffer_TTLRefreshedOnMultiplePush(t *testing.T) {
	cleanRedisKey(t, "wake:refresh-agent")
	buf := newRedisBuffer()
	ctx := context.Background()

	msg1 := messagebuffer.PendingMessage{Source: "telegram", From: "@matt", Text: "first", Timestamp: "2026-04-10T00:00:00Z"}
	if err := buf.Push(ctx, "refresh-agent", msg1); err != nil {
		t.Fatalf("Push(msg1): %v", err)
	}

	// Small sleep to let TTL tick down a bit.
	time.Sleep(100 * time.Millisecond)

	msg2 := messagebuffer.PendingMessage{Source: "telegram", From: "@matt", Text: "second", Timestamp: "2026-04-10T00:00:01Z"}
	if err := buf.Push(ctx, "refresh-agent", msg2); err != nil {
		t.Fatalf("Push(msg2): %v", err)
	}

	ttl, err := sharedRDB.TTL(ctx, "wake:refresh-agent").Result()
	if err != nil {
		t.Fatalf("TTL command: %v", err)
	}
	// After refresh, TTL must still be close to 5 minutes.
	const expectedTTL = 5 * time.Minute
	const tolerance = 10 * time.Second
	if ttl < expectedTTL-tolerance {
		t.Errorf("TTL not refreshed: got %v, expected ~%v", ttl, expectedTTL)
	}
}

// TestRedisBuffer_MessageOrder verifies messages are returned in push order (FIFO / list semantics).
func TestRedisBuffer_MessageOrder(t *testing.T) {
	cleanRedisKey(t, "wake:order-agent")
	buf := newRedisBuffer()
	ctx := context.Background()

	texts := []string{"a", "b", "c", "d", "e"}
	for i, text := range texts {
		msg := messagebuffer.PendingMessage{
			Source:    "telegram",
			From:      "@matt",
			Text:      text,
			Timestamp: "2026-04-10T00:00:0" + string(rune('0'+i)) + "Z",
		}
		if err := buf.Push(ctx, "order-agent", msg); err != nil {
			t.Fatalf("Push(%q): %v", text, err)
		}
	}

	got, err := buf.Drain(ctx, "order-agent")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(got) != len(texts) {
		t.Fatalf("Drain returned %d messages, want %d", len(got), len(texts))
	}
	for i, msg := range got {
		if msg.Text != texts[i] {
			t.Errorf("message[%d].Text = %q, want %q", i, msg.Text, texts[i])
		}
	}
}
