package api

import (
	"context"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/inbound"
	"github.com/matty-v/kyber/pkg/requeststore"
)

func TestRequestDeliveryTimeoutPersistsFailureBeforeStoreExpiry(t *testing.T) {
	limits := requeststore.DefaultLimits()
	limits.Lifetime = time.Second
	store, err := requeststore.NewMemoryStore(limits)
	if err != nil {
		t.Fatal(err)
	}
	request, err := store.Create(context.Background(), "kiosk", "req_deadline", "prompt", "")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{RequestStore: store}

	handler := inbound.NewDeliveryHandler(
		func(ctx context.Context, _ inbound.Job, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
		func(context.Context, inbound.Job) error {
			t.Fatal("expired Running-phase wait must not attempt delivery")
			return nil
		},
		3*time.Minute,
		30*time.Second,
	)
	handler(context.Background(), inbound.Job{
		Agent:         request.Agent,
		RequestID:     request.ID,
		Kind:          inbound.JobKindRequest,
		DeliverBefore: request.ExpiresAt,
		OnDelivery: func(ctx context.Context, outcome inbound.DeliveryOutcome) {
			server.recordRequestDelivery(ctx, request.Agent, request.ID, outcome)
		},
	})

	if !time.Now().Before(request.ExpiresAt) {
		t.Fatal("delivery outcome was reported only after store expiry")
	}
	stored, err := store.Get(context.Background(), request.Agent, request.ID)
	if err != nil {
		t.Fatalf("Get() after delivery timeout = %v", err)
	}
	if stored.Status != requeststore.StatusFailed || stored.FailureCode != requeststore.FailureAgentUnavailable {
		t.Fatalf("stored request = status %q failure %q", stored.Status, stored.FailureCode)
	}
}
