package inbound

import (
	"testing"
)

func TestRateLimiterBurst(t *testing.T) {
	r := NewRateLimiter()
	const max = 5

	allowed := 0
	for i := 0; i < max; i++ {
		if r.Allow("agent-a", "binding-a", max) {
			allowed++
		}
	}
	if allowed != max {
		t.Fatalf("first %d Allow calls should succeed, got %d", max, allowed)
	}
	// Burst of `max` is exhausted; next call must fail (token regen is paced
	// per minute, so within the same loop we don't expect refill).
	if r.Allow("agent-a", "binding-a", max) {
		t.Fatalf("Allow should reject after burst is consumed")
	}
}

func TestRateLimiterPerBindingIsolation(t *testing.T) {
	r := NewRateLimiter()
	const max = 2

	// Drain binding A.
	if !r.Allow("agent-a", "A", max) {
		t.Fatal("A token 1 should pass")
	}
	if !r.Allow("agent-a", "A", max) {
		t.Fatal("A token 2 should pass")
	}
	if r.Allow("agent-a", "A", max) {
		t.Fatal("A should be exhausted")
	}
	// Binding B for the same agent must still have a full bucket.
	if !r.Allow("agent-a", "B", max) {
		t.Fatal("B token 1 should pass — bindings are isolated")
	}
}

func TestRateLimiterPerAgentIsolation(t *testing.T) {
	r := NewRateLimiter()
	const max = 1

	if !r.Allow("agent-a", "x", max) {
		t.Fatal("agent-a token 1 should pass")
	}
	if r.Allow("agent-a", "x", max) {
		t.Fatal("agent-a should be exhausted")
	}
	if !r.Allow("agent-b", "x", max) {
		t.Fatal("agent-b token 1 should pass — agents are isolated")
	}
}

func TestRateLimiterMaxPerMinuteChangeResetsBucket(t *testing.T) {
	r := NewRateLimiter()
	if !r.Allow("a", "b", 1) {
		t.Fatal("first call with max=1 should pass")
	}
	if r.Allow("a", "b", 1) {
		t.Fatal("second call with max=1 should fail (bucket exhausted)")
	}
	// Operator raised the limit — bucket should be rebuilt with full burst.
	if !r.Allow("a", "b", 5) {
		t.Fatal("call with new max=5 should pass — bucket reset on limit change")
	}
}

func TestRateLimiterDefaultsWhenZero(t *testing.T) {
	r := NewRateLimiter()
	// Spec says default 10/min when unset; first 10 should pass without ever
	// calling with maxPerMinute > 0.
	allowed := 0
	for i := 0; i < defaultMaxPerMinute; i++ {
		if r.Allow("x", "y", 0) {
			allowed++
		}
	}
	if allowed != defaultMaxPerMinute {
		t.Fatalf("expected %d allowed with default, got %d", defaultMaxPerMinute, allowed)
	}
	if r.Allow("x", "y", 0) {
		t.Fatalf("should be exhausted at default cap")
	}
}
