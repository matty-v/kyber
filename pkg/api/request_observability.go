package api

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/matty-v/kyber/pkg/telemetry"
)

func recordAgentRequestEvent(ctx context.Context, event, outcome, agent, requestID string) {
	if telemetry.AgentRequestsTotal != nil {
		telemetry.AgentRequestsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("event", event), attribute.String("outcome", outcome)))
	}
	slog.Info("agent request audit", "event", event, "outcome", outcome,
		"agent", agent, "request_id", requestID)
}

func recordAgentRequestTerminal(ctx context.Context, event, outcome, agent, requestID string, createdAt time.Time) {
	recordAgentRequestEvent(ctx, event, outcome, agent, requestID)
	if telemetry.AgentRequestDuration != nil && !createdAt.IsZero() {
		telemetry.AgentRequestDuration.Record(ctx, time.Since(createdAt).Seconds(),
			metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}
