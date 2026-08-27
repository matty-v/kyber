package inbound

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeliveryHandler_RequestOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		deadline    time.Time
		waitErr     error
		deliverErr  error
		wantOutcome DeliveryOutcome
		wantDeliver bool
	}{
		{
			name:        "running agent is dispatched",
			deadline:    time.Now().Add(time.Minute),
			wantOutcome: DeliveryDispatched,
			wantDeliver: true,
		},
		{
			name:        "agent unavailable before expiry",
			deadline:    time.Now().Add(time.Minute),
			waitErr:     errors.New("not running"),
			wantOutcome: DeliveryAgentUnavailable,
		},
		{
			name:        "tmux delivery fails",
			deadline:    time.Now().Add(time.Minute),
			deliverErr:  errors.New("exec failed"),
			wantOutcome: DeliveryFailed,
			wantDeliver: true,
		},
		{
			name:        "request already expired",
			deadline:    time.Now().Add(-time.Second),
			wantOutcome: DeliveryAgentUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var delivered bool
			var got DeliveryOutcome
			handler := NewDeliveryHandler(
				func(context.Context, Job, time.Duration) error { return tc.waitErr },
				func(context.Context, Job) error {
					delivered = true
					return tc.deliverErr
				},
				time.Minute,
				30*time.Second,
			)
			handler(context.Background(), Job{
				Kind:          JobKindRequest,
				DeliverBefore: tc.deadline,
				OnDelivery: func(_ context.Context, outcome DeliveryOutcome) {
					got = outcome
				},
			})
			if delivered != tc.wantDeliver {
				t.Fatalf("delivered = %v, want %v", delivered, tc.wantDeliver)
			}
			if got != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", got, tc.wantOutcome)
			}
		})
	}
}

func TestDeliveryHandler_WebhookPreservesAttemptAfterGateFailure(t *testing.T) {
	var delivered bool
	handler := NewDeliveryHandler(
		func(context.Context, Job, time.Duration) error { return errors.New("not running") },
		func(context.Context, Job) error {
			delivered = true
			return nil
		},
		time.Minute,
		30*time.Second,
	)
	handler(context.Background(), Job{})
	if !delivered {
		t.Fatal("webhook delivery must still be attempted after gate failure")
	}
}

func TestDeliveryHandler_RequestWaitIsBoundedByExpiry(t *testing.T) {
	deadline := time.Now().Add(10 * time.Second)
	var gotTimeout time.Duration
	handler := NewDeliveryHandler(
		func(_ context.Context, _ Job, timeout time.Duration) error {
			gotTimeout = timeout
			return errors.New("not running")
		},
		func(context.Context, Job) error { return nil },
		3*time.Minute,
		30*time.Second,
	)
	handler(context.Background(), Job{Kind: JobKindRequest, DeliverBefore: deadline})
	if gotTimeout <= 0 || gotTimeout > 5*time.Second {
		t.Fatalf("request wait = %s, want expiry minus persistence reserve", gotTimeout)
	}
}
