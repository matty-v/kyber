package nodeagent

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// defaultOTLPEndpoint is used when OTEL_EXPORTER_OTLP_ENDPOINT is not set.
const defaultOTLPEndpoint = "http://localhost:4318"

// defaultServiceName is the OTEL service name for the node agent.
const defaultServiceName = "kyber-node-agent"

// ShutdownFunc is a function that flushes and shuts down the OTEL SDK.
type ShutdownFunc func(ctx context.Context) error

// Exporter holds OTEL meter instruments used by the metrics push loop.
type Exporter struct {
	meter    metric.Meter
	nodeName string

	cpuPercent   metric.Float64Gauge
	memUsed      metric.Int64Gauge
	memTotal     metric.Int64Gauge
	diskUsed     metric.Int64Gauge
	diskTotal    metric.Int64Gauge
	uptimeSecs   metric.Int64Gauge
	load1        metric.Float64Gauge
	load5        metric.Float64Gauge
	load15       metric.Float64Gauge
}

// NewOTELExporter initialises the OTEL SDK, registers all gauge instruments, and
// returns an Exporter plus a shutdown function. Configuration is read from env vars:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT — default: http://localhost:4318
//	OTEL_SERVICE_NAME            — default: kyber-node-agent
//	KYBER_NODE_NAME              — the k8s node name (from DaemonSet downward API)
func NewOTELExporter(ctx context.Context) (*Exporter, ShutdownFunc, error) {
	endpoint := getEnvOrDefault("OTEL_EXPORTER_OTLP_ENDPOINT", defaultOTLPEndpoint)
	serviceName := getEnvOrDefault("OTEL_SERVICE_NAME", defaultServiceName)
	nodeName := os.Getenv("KYBER_NODE_NAME")

	// OTLP HTTP exporter. The SDK reads OTEL_EXPORTER_OTLP_ENDPOINT from the
	// environment automatically; we also pass it explicitly so tests can override.
	exp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(endpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("creating OTLP HTTP exporter: %w", err)
	}

	// Resource identifies this service in the OTEL backend.
	//
	// Use resource.New instead of resource.Merge(resource.Default(), ...) to
	// avoid schema-URL conflicts between the SDK's default resource (shipped
	// with a newer semconv schema URL) and our own semconv import (which may
	// pin an older schema version). resource.Default()+Merge fails with
	// "conflicting Schema URL" at runtime — seen in the D2 telemetry work
	// and reproduced here on the node-agent hot path.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("creating OTEL resource: %w", err)
	}

	// Metric provider with a push reader (60s default; the loop controls actual cadence).
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(60*time.Second)),
		),
	)
	otel.SetMeterProvider(provider)

	meter := provider.Meter("kyber.node-agent")

	ex := &Exporter{meter: meter, nodeName: nodeName}
	if err := ex.registerInstruments(); err != nil {
		_ = provider.Shutdown(ctx)
		return nil, nil, err
	}

	shutdown := func(ctx context.Context) error {
		return provider.Shutdown(ctx)
	}
	return ex, shutdown, nil
}

// registerInstruments creates all gauge instruments. Returns an error if any
// instrument registration fails (which indicates a programming error, not a
// runtime failure).
func (e *Exporter) registerInstruments() error {
	var err error

	if e.cpuPercent, err = e.meter.Float64Gauge("kyber_node_cpu_percent",
		metric.WithDescription("CPU utilization percentage over the last collection interval"),
		metric.WithUnit("%"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_cpu_percent: %w", err)
	}

	if e.memUsed, err = e.meter.Int64Gauge("kyber_node_memory_used_bytes",
		metric.WithDescription("Memory in use, in bytes"),
		metric.WithUnit("By"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_memory_used_bytes: %w", err)
	}

	if e.memTotal, err = e.meter.Int64Gauge("kyber_node_memory_total_bytes",
		metric.WithDescription("Total physical memory, in bytes"),
		metric.WithUnit("By"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_memory_total_bytes: %w", err)
	}

	if e.diskUsed, err = e.meter.Int64Gauge("kyber_node_disk_used_bytes",
		metric.WithDescription("Disk space used on the root filesystem, in bytes"),
		metric.WithUnit("By"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_disk_used_bytes: %w", err)
	}

	if e.diskTotal, err = e.meter.Int64Gauge("kyber_node_disk_total_bytes",
		metric.WithDescription("Total disk space on the root filesystem, in bytes"),
		metric.WithUnit("By"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_disk_total_bytes: %w", err)
	}

	if e.uptimeSecs, err = e.meter.Int64Gauge("kyber_node_uptime_seconds",
		metric.WithDescription("Seconds since the node last booted"),
		metric.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_uptime_seconds: %w", err)
	}

	if e.load1, err = e.meter.Float64Gauge("kyber_node_load1",
		metric.WithDescription("1-minute load average"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_load1: %w", err)
	}

	if e.load5, err = e.meter.Float64Gauge("kyber_node_load5",
		metric.WithDescription("5-minute load average"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_load5: %w", err)
	}

	if e.load15, err = e.meter.Float64Gauge("kyber_node_load15",
		metric.WithDescription("15-minute load average"),
	); err != nil {
		return fmt.Errorf("registering kyber_node_load15: %w", err)
	}

	return nil
}

// Record emits all metrics from m to the OTEL meter.
func (e *Exporter) Record(ctx context.Context, m *Metrics) {
	attrs := metric.WithAttributes(attribute.String("node_name", e.nodeName))

	e.cpuPercent.Record(ctx, m.CPUPercent, attrs)
	e.memUsed.Record(ctx, int64(m.MemoryUsed), attrs)    //nolint:gosec
	e.memTotal.Record(ctx, int64(m.MemoryTotal), attrs)  //nolint:gosec
	e.diskUsed.Record(ctx, int64(m.DiskUsed), attrs)     //nolint:gosec
	e.diskTotal.Record(ctx, int64(m.DiskTotal), attrs)   //nolint:gosec
	e.uptimeSecs.Record(ctx, m.UptimeSecs, attrs)
	e.load1.Record(ctx, m.Load1, attrs)
	e.load5.Record(ctx, m.Load5, attrs)
	e.load15.Record(ctx, m.Load15, attrs)
}

// RunMetricsLoop collects and exports metrics on the given interval until ctx is cancelled.
// Export failures are logged and do not crash the loop — a transient OTEL collector
// outage should not bring down the node agent. reporter may be nil (OTEL-only mode).
func RunMetricsLoop(ctx context.Context, collector *Collector, exporter *Exporter, interval time.Duration, reporter *ResourceReporter) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m, err := collector.Collect()
			if err != nil {
				log.Printf("node-agent: metrics collection failed: %v", err)
				continue
			}
			exporter.Record(ctx, m)
			reporter.Report(ctx, m)
		}
	}
}

// getEnvOrDefault returns the value of the environment variable key,
// or fallback if the variable is unset or empty.
func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
