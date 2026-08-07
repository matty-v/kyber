package inbound

import (
	"context"
	"testing"
	"time"
)

func TestMemoryEnvelopeCache_PutGetRoundTrip(t *testing.T) {
	c := NewMemoryEnvelopeCache()
	ctx := context.Background()
	if err := c.Put(ctx, "req-1", "envelope-body"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx, "req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "envelope-body" {
		t.Fatalf("Get returned %q, want %q", got, "envelope-body")
	}
}

func TestMemoryEnvelopeCache_GetUnknownReturnsEmpty(t *testing.T) {
	c := NewMemoryEnvelopeCache()
	got, err := c.Get(context.Background(), "never-written")
	if err != nil {
		t.Fatalf("Get: %v (want nil)", err)
	}
	if got != "" {
		t.Fatalf("Get returned %q, want empty string for unknown key", got)
	}
}

func TestMemoryEnvelopeCache_TTLEviction(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := NewMemoryEnvelopeCache()
	c.now = func() time.Time { return now }
	c.ttl = time.Second

	if err := c.Put(context.Background(), "req-1", "expiring"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Within TTL — visible.
	got, err := c.Get(context.Background(), "req-1")
	if err != nil {
		t.Fatalf("Get within TTL: %v", err)
	}
	if got != "expiring" {
		t.Fatalf("within TTL: got %q, want %q", got, "expiring")
	}

	// Past TTL — evicted, returns "" with no error.
	now = now.Add(2 * time.Second)
	got, err = c.Get(context.Background(), "req-1")
	if err != nil {
		t.Fatalf("Get past TTL: %v", err)
	}
	if got != "" {
		t.Fatalf("past TTL: got %q, want empty (evicted)", got)
	}

	// And the entry was actually removed from the map (lazy eviction).
	if _, ok := c.entries["req-1"]; ok {
		t.Errorf("entry should have been evicted from map after expired Get")
	}
}

func TestMemoryEnvelopeCache_SeparateKeysIsolated(t *testing.T) {
	c := NewMemoryEnvelopeCache()
	ctx := context.Background()
	_ = c.Put(ctx, "a", "envelope-a")
	_ = c.Put(ctx, "b", "envelope-b")

	gotA, _ := c.Get(ctx, "a")
	gotB, _ := c.Get(ctx, "b")
	if gotA != "envelope-a" || gotB != "envelope-b" {
		t.Fatalf("isolation broken: a=%q b=%q", gotA, gotB)
	}
}

func TestMemoryEnvelopeCache_PutOverwrites(t *testing.T) {
	c := NewMemoryEnvelopeCache()
	ctx := context.Background()
	_ = c.Put(ctx, "req-1", "v1")
	_ = c.Put(ctx, "req-1", "v2")
	got, _ := c.Get(ctx, "req-1")
	if got != "v2" {
		t.Fatalf("expected last-write-wins; got %q", got)
	}
}
