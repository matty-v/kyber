package requeststore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLimitsValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Limits)
	}{
		{"zero lifetime", func(l *Limits) { l.Lifetime = 0 }},
		{"lifetime above cap", func(l *Limits) { l.Lifetime = HardMaxLifetime + time.Second }},
		{"zero prompt bytes", func(l *Limits) { l.MaxPromptBytes = 0 }},
		{"prompt bytes above cap", func(l *Limits) { l.MaxPromptBytes = HardMaxPromptBytes + 1 }},
		{"zero correlation bytes", func(l *Limits) { l.MaxCorrelationBytes = 0 }},
		{"correlation bytes above cap", func(l *Limits) { l.MaxCorrelationBytes = HardMaxCorrelationBytes + 1 }},
		{"zero response bytes", func(l *Limits) { l.MaxResponseBytes = 0 }},
		{"response bytes above cap", func(l *Limits) { l.MaxResponseBytes = HardMaxResponseBytes + 1 }},
		{"zero outstanding", func(l *Limits) { l.MaxOutstanding = 0 }},
		{"outstanding above cap", func(l *Limits) { l.MaxOutstanding = HardMaxOutstanding + 1 }},
		{"zero terminal", func(l *Limits) { l.MaxTerminal = 0 }},
		{"terminal above cap", func(l *Limits) { l.MaxTerminal = HardMaxTerminal + 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limits := DefaultLimits()
			tc.mutate(&limits)
			if err := limits.Validate(); !errors.Is(err, ErrInvalidLimits) {
				t.Fatalf("Validate() error = %v, want %v", err, ErrInvalidLimits)
			}
		})
	}
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatalf("DefaultLimits().Validate() error = %v", err)
	}
}

func TestMemoryStoreLifecycleAndIdempotency(t *testing.T) {
	store := newTestMemoryStore(t, DefaultLimits())
	ctx := context.Background()
	created, err := store.Create(ctx, "kiosk", "req_1", "tell me", "caller-1")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.Status != StatusQueued || created.Agent != "kiosk" || created.Correlation != "caller-1" {
		t.Fatalf("Create() = %+v", created)
	}
	if err := store.MarkDispatched(ctx, "kiosk", "req_1"); err != nil {
		t.Fatalf("MarkDispatched() error = %v", err)
	}
	if err := store.MarkDispatched(ctx, "kiosk", "req_1"); err != nil {
		t.Fatalf("idempotent MarkDispatched() error = %v", err)
	}
	if err := store.Complete(ctx, "kiosk", "req_1", "answer"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if err := store.Complete(ctx, "kiosk", "req_1", "answer"); err != nil {
		t.Fatalf("idempotent Complete() error = %v", err)
	}
	if err := store.Complete(ctx, "kiosk", "req_1", "different"); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Complete() error = %v, want %v", err, ErrConflict)
	}
	got, err := store.Get(ctx, "kiosk", "req_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusCompleted || got.Response != "answer" || got.ExpiresAt.Sub(got.CreatedAt) != DefaultLifetime {
		t.Fatalf("Get() = %+v", got)
	}
	got.Response = "mutated"
	again, err := store.Get(ctx, "kiosk", "req_1")
	if err != nil || again.Response != "answer" {
		t.Fatalf("defensive Get() = %+v, %v", again, err)
	}
}

func TestMemoryStoreTransitionGuards(t *testing.T) {
	store := newTestMemoryStore(t, DefaultLimits())
	ctx := context.Background()
	if _, err := store.Create(ctx, "kiosk", "req_1", "prompt", ""); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.Complete(ctx, "kiosk", "req_1", "too soon"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Complete(queued) error = %v, want %v", err, ErrConflict)
	}
	if err := store.Fail(ctx, "kiosk", "req_1", FailureDelivery); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	if err := store.Fail(ctx, "kiosk", "req_1", FailureDelivery); err != nil {
		t.Fatalf("idempotent Fail() error = %v", err)
	}
	if err := store.Fail(ctx, "kiosk", "req_1", FailureAgentUnavailable); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Fail() error = %v, want %v", err, ErrConflict)
	}
	if err := store.MarkDispatched(ctx, "kiosk", "req_1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkDispatched(failed) error = %v, want %v", err, ErrConflict)
	}
	got, err := store.Get(ctx, "kiosk", "req_1")
	if err != nil || got.Status != StatusFailed || got.FailureCode != FailureDelivery {
		t.Fatalf("Get() = %+v, %v", got, err)
	}
}

func TestMemoryStoreBoundsPayloadsByBytes(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPromptBytes = 4
	limits.MaxCorrelationBytes = 4
	limits.MaxResponseBytes = 4
	store := newTestMemoryStore(t, limits)
	ctx := context.Background()
	if _, err := store.Create(ctx, "kiosk", "too-big", "ééé", ""); !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("Create(oversize) error = %v, want %v", err, ErrPromptTooLarge)
	}
	if _, err := store.Create(ctx, "kiosk", "correlation-ascii", "four", "1234"); err != nil {
		t.Fatalf("Create(correlation ASCII boundary) error = %v", err)
	}
	if err := store.Fail(ctx, "kiosk", "correlation-ascii", FailureDelivery); err != nil {
		t.Fatalf("Fail(correlation ASCII boundary) error = %v", err)
	}
	if _, err := store.Create(ctx, "kiosk", "correlation-utf8", "four", "éé"); err != nil {
		t.Fatalf("Create(correlation UTF-8 boundary) error = %v", err)
	}
	if err := store.Fail(ctx, "kiosk", "correlation-utf8", FailureDelivery); err != nil {
		t.Fatalf("Fail(correlation UTF-8 boundary) error = %v", err)
	}
	if _, err := store.Create(ctx, "kiosk", "correlation-too-big-ascii", "four", "12345"); !errors.Is(err, ErrCorrelationTooLarge) {
		t.Fatalf("Create(oversize ASCII correlation) error = %v, want %v", err, ErrCorrelationTooLarge)
	}
	if _, err := store.Create(ctx, "kiosk", "correlation-too-big-utf8", "four", "ééé"); !errors.Is(err, ErrCorrelationTooLarge) {
		t.Fatalf("Create(oversize UTF-8 correlation) error = %v, want %v", err, ErrCorrelationTooLarge)
	}
	if _, err := store.Create(ctx, "kiosk", "req_1", "four", ""); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.MarkDispatched(ctx, "kiosk", "req_1"); err != nil {
		t.Fatalf("MarkDispatched() error = %v", err)
	}
	if err := store.Complete(ctx, "kiosk", "req_1", "ééé"); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Complete(oversize) error = %v, want %v", err, ErrResponseTooLarge)
	}
	got, err := store.Get(ctx, "kiosk", "req_1")
	if err != nil || got.Status != StatusDispatched {
		t.Fatalf("oversize response changed request: %+v, %v", got, err)
	}
}

func TestMemoryStoreOutstandingLimitIsAtomic(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxOutstanding = 2
	store := newTestMemoryStore(t, limits)
	ctx := context.Background()
	const attempts = 32
	var wg sync.WaitGroup
	errorsCh := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Create(ctx, "kiosk", requestID(i), "prompt", "")
			errorsCh <- err
		}(i)
	}
	wg.Wait()
	close(errorsCh)
	succeeded := 0
	limited := 0
	for err := range errorsCh {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrOutstandingLimit):
			limited++
		default:
			t.Fatalf("Create() unexpected error = %v", err)
		}
	}
	if succeeded != 2 || limited != attempts-2 {
		t.Fatalf("Create() successes = %d, limited = %d", succeeded, limited)
	}
}

func TestMemoryStoreExpiryReleasesCapacity(t *testing.T) {
	limits := DefaultLimits()
	limits.Lifetime = time.Second
	limits.MaxOutstanding = 1
	store := newTestMemoryStore(t, limits)
	ctx := context.Background()
	if _, err := store.Create(ctx, "kiosk", "req_1", "prompt", ""); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := store.Create(ctx, "kiosk", "req_2", "prompt", ""); !errors.Is(err, ErrOutstandingLimit) {
		t.Fatalf("Create(at limit) error = %v, want %v", err, ErrOutstandingLimit)
	}
	now := store.now()
	store.now = func() time.Time { return now.Add(time.Second) }
	if _, err := store.Get(ctx, "kiosk", "req_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(expired) error = %v, want %v", err, ErrNotFound)
	}
	if _, err := store.Create(ctx, "kiosk", "req_2", "prompt", ""); err != nil {
		t.Fatalf("Create(after expiry) error = %v", err)
	}
	if err := store.Complete(ctx, "kiosk", "req_1", "late"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Complete(expired) error = %v, want %v", err, ErrNotFound)
	}
}

func TestMemoryStoreTerminalRetention(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxOutstanding = 3
	limits.MaxTerminal = 2
	store := newTestMemoryStore(t, limits)
	ctx := context.Background()
	base := store.now()
	for i := 1; i <= 3; i++ {
		store.now = func() time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
		id := requestID(i)
		if _, err := store.Create(ctx, "kiosk", id, "prompt", ""); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
		if err := store.Fail(ctx, "kiosk", id, FailureDelivery); err != nil {
			t.Fatalf("Fail(%s) error = %v", id, err)
		}
	}
	if _, err := store.Get(ctx, "kiosk", requestID(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(oldest) error = %v, want %v", err, ErrNotFound)
	}
	for i := 2; i <= 3; i++ {
		if _, err := store.Get(ctx, "kiosk", requestID(i)); err != nil {
			t.Fatalf("Get(retained %d) error = %v", i, err)
		}
	}
}

func newTestMemoryStore(t *testing.T, limits Limits) *MemoryStore {
	t.Helper()
	store, err := NewMemoryStore(limits)
	if err != nil {
		t.Fatalf("NewMemoryStore() error = %v", err)
	}
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	return store
}

func requestID(i int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if i < len(digits) {
		return "req_" + string(digits[i])
	}
	return "req_x" + string(digits[i%len(digits)])
}
