// Package telemetry provides OTEL SDK initialization and metric definitions
// for the Kyber control plane. It follows the same pattern as the node agent's
// OTEL setup (pkg/nodeagent/otel.go) so both components share consistent
// configuration conventions and SDK versions.
package telemetry

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config holds telemetry initialization parameters. Values are typically sourced
// from environment variables (see Init) but can be set directly in tests.
type Config struct {
	// Enabled controls whether the real OTEL SDK is initialised. When false,
	// Init returns no-op providers so callers never need nil checks.
	Enabled bool

	// Endpoint is the OTLP HTTP exporter endpoint, e.g. "http://otel-collector:4318".
	// Init also reads OTEL_EXPORTER_OTLP_ENDPOINT; the env var takes precedence.
	Endpoint string

	// ServiceName is the OTEL service.name resource attribute.
	ServiceName string

	// ServiceVersion is the OTEL service.version resource attribute.
	ServiceVersion string
}

// Telemetry holds live SDK providers and a combined shutdown function.
// Call Shutdown before process exit to flush pending spans and metrics.
type Telemetry struct {
	MeterProvider  *sdkmetric.MeterProvider
	TracerProvider *sdktrace.TracerProvider

	// Shutdown flushes and shuts down both providers. Safe to call multiple times.
	Shutdown func(ctx context.Context) error
}

// Init initialises the OTEL SDK according to cfg and returns a Telemetry bundle.
//
// Endpoint resolution (first match wins):
//  1. OTEL_EXPORTER_OTLP_ENDPOINT environment variable
//  2. cfg.Endpoint
//  3. disabled (no endpoint → fall through to noop)
//
// When cfg.Enabled is false, or when no endpoint is available, Init returns a
// no-op Telemetry so all downstream code paths work without nil checks.
func Init(ctx context.Context, cfg Config) (*Telemetry, error) {
	// Env var takes precedence over config value — same convention as the node agent.
	if ep := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); ep != "" {
		cfg.Endpoint = ep
	}

	if !cfg.Enabled || cfg.Endpoint == "" {
		return noopTelemetry(), nil
	}

	// Resource identifies this service in the OTEL backend.
	// resource.Merge can fail when schema URLs differ between Default() (which uses
	// the SDK's bundled semconv version) and the explicit semconv import used here.
	// We use resource.New with explicit attributes to avoid the schema mismatch.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTEL resource: %w", err)
	}

	// --- Metric provider ---
	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(cfg.Endpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(60*time.Second)),
		),
	)

	// --- Trace provider ---
	traceExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		// Clean up the metric provider we already created.
		_ = mp.Shutdown(ctx)
		return nil, fmt.Errorf("creating OTLP trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
	)

	// Register as global providers so third-party libraries pick them up automatically.
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)

	return &Telemetry{
		MeterProvider:  mp,
		TracerProvider: tp,
		Shutdown: func(ctx context.Context) error {
			_ = mp.Shutdown(ctx)
			_ = tp.Shutdown(ctx)
			return nil
		},
	}, nil
}

// noopTelemetry returns a Telemetry whose providers are no-ops. All OTEL API
// calls (Add, Record, Start) are still valid; they simply do nothing. This
// avoids nil-check boilerplate throughout the codebase.
func noopTelemetry() *Telemetry {
	// Create SDK providers with no exporters or readers — they drop all data.
	mp := sdkmetric.NewMeterProvider()
	tp := sdktrace.NewTracerProvider() // no exporters → silent drop

	return &Telemetry{
		MeterProvider:  mp,
		TracerProvider: tp,
		Shutdown: func(ctx context.Context) error {
			_ = mp.Shutdown(ctx)
			_ = tp.Shutdown(ctx)
			return nil
		},
	}
}
