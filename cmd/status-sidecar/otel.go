// OTel metrics emission for kyber-status-sidecar (kyber#256, #247 Phase C1).
//
// The sidecar pushes per-event signals to the control plane (kyber#248/249);
// this file adds a parallel per-agent metrics export to the same OTel collector
// the kyber-node-agent talks to. Closes the cumulative active/idle time half
// of #174 (state half closed by #249).
//
// Mirrors pkg/nodeagent/otel.go's init pattern: OTLP HTTP exporter, periodic
// reader, scheme-conflict-safe resource construction. Different choices made
// here vs node-agent:
//   - 30s push interval (instead of 60s) — matches the issue's "every 30s"
//     contract; agent activity is bursty, finer cadence beats node metrics
//   - The endpoint env var is KYBER_OTEL_ENDPOINT (not OTEL_EXPORTER_OTLP_ENDPOINT)
//     so the chart can independently gate sidecar metrics without affecting
//     other binaries' OTLP behavior
//   - All instruments are owned by *Metrics in activity_metrics.go — this file
//     only stands up the meter
//
// kyber#360 Cause E: the activity-state machine MUST work even when the OTel
// exporter is disabled (the production default). Before this fix,
// initOTel returned a nil *Metrics for empty endpoints; every nil-receiver
// method then silently no-opped, so BumpActivityCounter never ran and Redis
// stayed empty. The fix decouples state-tracking from export by always
// constructing *Metrics — backed by an OTLP exporter when configured, by a
// noop MeterProvider otherwise. The in-memory bookkeeping (stateSecs,
// pendingTransitions, currentState) is unaffected by which provider sits
// behind the instrument handles.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	// otelPushInterval is the cadence at which buffered counters/gauges flush
	// to the collector. The issue contract is "every 30s"; tighter would
	// drown the collector at fleet scale, looser would smear activity-state
	// transitions across longer windows.
	otelPushInterval = 30 * time.Second

	// otelServiceName lands as service.name on every metric. Matches the
	// kyber-node-agent convention (service per binary, not per pod).
	otelServiceName = "kyber-status-sidecar"
)

// otelShutdown flushes pending metrics and tears down the SDK.
type otelShutdown func(ctx context.Context) error

// initOTel always returns a usable *Metrics. When KYBER_OTEL_ENDPOINT is set
// and the OTLP exporter wires up cleanly, the *Metrics drives a real periodic
// reader. Otherwise it sits on top of a noop MeterProvider — the in-memory
// state machine that feeds postMetricsSnapshot (see activity_metrics.go) is
// independent of whether anything is exported, so panels keep populating even
// in dev installs without a collector.
//
// Returns a non-nil error only when (a) the operator configured an endpoint
// but the OTLP wiring failed (export disabled, state machine still active)
// or (b) instrument registration tripped a programming bug (newMetrics
// returned an error — the only crash-worthy case).
func initOTel(ctx context.Context, endpoint string) (*Metrics, otelShutdown, error) {
	var (
		provider    metric.MeterProvider = noop.NewMeterProvider()
		shutdown    otelShutdown         = func(context.Context) error { return nil }
		exporterErr error
	)

	if endpoint != "" {
		sdkProvider, sdkShutdown, err := buildOTLPProvider(ctx, endpoint)
		if err != nil {
			// Fall through with the noop provider so the activity-state
			// machine keeps working — kyber#360 Cause E was about precisely
			// this branch silently disabling state tracking. The caller logs
			// the error; the rest of the sidecar carries on.
			exporterErr = err
		} else {
			provider = sdkProvider
			shutdown = sdkShutdown
			otel.SetMeterProvider(sdkProvider)
		}
	}

	meter := provider.Meter("kyber.status-sidecar")
	m, err := newMetrics(meter)
	if err != nil {
		// Instrument registration shouldn't fail — counter-name conflict is a
		// programming bug, not a runtime condition. Surface it so the binary
		// crashes loudly instead of running with a half-constructed *Metrics.
		_ = shutdown(ctx)
		return nil, func(context.Context) error { return nil }, err
	}
	return m, shutdown, exporterErr
}

// buildOTLPProvider stands up the SDK-backed MeterProvider that pushes to
// the configured OTLP HTTP endpoint. Split out from initOTel so the empty-
// endpoint and exporter-failure paths share the same fall-through onto the
// noop provider in one place.
func buildOTLPProvider(ctx context.Context, endpoint string) (*sdkmetric.MeterProvider, otelShutdown, error) {
	exp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(endpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("otlp http exporter: %w", err)
	}

	// resource.New (not Default+Merge) — the latter trips a schema-URL
	// conflict between the SDK's bundled semconv and our pinned import.
	// Same gotcha pkg/nodeagent/otel.go documents.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(otelServiceName),
			semconv.ServiceInstanceID(os.Getenv("AGENT_NAME")),
		),
	)
	if err != nil {
		_ = exp.Shutdown(ctx)
		return nil, nil, fmt.Errorf("otel resource: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(otelPushInterval)),
		),
	)
	return provider, provider.Shutdown, nil
}
