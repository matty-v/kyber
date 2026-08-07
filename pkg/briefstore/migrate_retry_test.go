package briefstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMigrateWithRetry_SucceedsFirstAttempt(t *testing.T) {
	calls := 0
	err := MigrateWithRetry(context.Background(), func(context.Context) error {
		calls++
		return nil
	}, 200*time.Millisecond, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls: got %d, want 1", calls)
	}
}

func TestMigrateWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	fails := 3
	calls := 0
	err := MigrateWithRetry(context.Background(), func(context.Context) error {
		calls++
		if calls <= fails {
			return errors.New("connection refused")
		}
		return nil
	}, 2*time.Second, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != fails+1 {
		t.Errorf("calls: got %d, want %d", calls, fails+1)
	}
}

func TestMigrateWithRetry_ReturnsLastErrorOnBudgetExhausted(t *testing.T) {
	calls := 0
	boom := errors.New("still down")
	err := MigrateWithRetry(context.Background(), func(context.Context) error {
		calls++
		return boom
	}, 50*time.Millisecond, 10*time.Millisecond, nil)
	if !errors.Is(err, boom) {
		t.Errorf("error: got %v, want %v", err, boom)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 attempts within budget, got %d", calls)
	}
}

func TestMigrateWithRetry_HonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	errCh := make(chan error, 1)
	go func() {
		errCh <- MigrateWithRetry(ctx, func(context.Context) error {
			calls++
			return errors.New("nope")
		}, 10*time.Second, 50*time.Millisecond, nil)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry loop did not exit after cancel")
	}
}
