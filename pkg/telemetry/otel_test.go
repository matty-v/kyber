package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestInit_Disabled(t *testing.T) {
	ctx := context.Background()

	tel, err := Init(ctx, Config{
		Enabled:        false,
		ServiceName:    "test",
		ServiceVersion: "0.0.1",
	})
	if err != nil {
		t.Fatalf("Init(disabled) returned error: %v", err)
	}
	if tel == nil {
		t.Fatal("Init(disabled) returned nil Telemetry")
	}
	if tel.MeterProvider == nil {
		t.Error("MeterProvider should be non-nil (noop)")
	}
	if tel.TracerProvider == nil {
		t.Error("TracerProvider should be non-nil (noop)")
	}
	if tel.Shutdown == nil {
		t.Error("Shutdown func should be non-nil")
	}

	// Shutdown should not error even for a noop provider.
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := tel.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown(disabled) returned error: %v", err)
	}
}

func TestInit_EnabledNoEndpoint(t *testing.T) {
	ctx := context.Background()

	// Enabled=true but no endpoint configured → falls back to noop.
	tel, err := Init(ctx, Config{
		Enabled:     true,
		Endpoint:    "",
		ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("Init(enabled, no endpoint) returned error: %v", err)
	}
	if tel == nil {
		t.Fatal("Init(enabled, no endpoint) returned nil Telemetry")
	}
	// Should be noop, not nil.
	if tel.MeterProvider == nil || tel.TracerProvider == nil {
		t.Error("providers should be non-nil even when falling back to noop")
	}
}

func TestInit_EnabledBadEndpoint(t *testing.T) {
	ctx := context.Background()

	// Enabled with a syntactically valid but unreachable endpoint. The SDK
	// initialises successfully and exports asynchronously, so Init should
	// not block or error — failures surface at export time, not init time.
	tel, err := Init(ctx, Config{
		Enabled:     true,
		Endpoint:    "http://127.0.0.1:19999", // nothing listening
		ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("Init(bad endpoint) returned error: %v", err)
	}
	if tel == nil {
		t.Fatal("Init(bad endpoint) returned nil Telemetry")
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := tel.Shutdown(shutdownCtx); err != nil {
		t.Logf("Shutdown returned (non-fatal): %v", err)
	}
}

func TestInit_EnvVarOverridesConfig(t *testing.T) {
	// Set the env var to empty, config has empty endpoint — should be noop.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	ctx := context.Background()
	tel, err := Init(ctx, Config{
		Enabled:     true,
		Endpoint:    "", // both empty → noop
		ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tel == nil {
		t.Fatal("Init returned nil")
	}
}

func TestShutdown_Idempotent(t *testing.T) {
	ctx := context.Background()
	tel, err := Init(ctx, Config{Enabled: false})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Call Shutdown twice — must not panic.
	_ = tel.Shutdown(shutdownCtx)
	_ = tel.Shutdown(shutdownCtx)
}
