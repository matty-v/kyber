package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    slog.Level
		wantErr bool
	}{
		{name: "empty defaults to info", value: "", want: slog.LevelInfo},
		{name: "case and whitespace", value: " DEBUG ", want: slog.LevelDebug},
		{name: "info", value: "info", want: slog.LevelInfo},
		{name: "warn", value: "warn", want: slog.LevelWarn},
		{name: "error", value: "error", want: slog.LevelError},
		{name: "invalid", value: "trace", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLevel(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseLevel(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestNewWritesStandardJSON(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(Config{
		Component:  "status-sidecar",
		Level:      "debug",
		Writer:     &output,
		Attributes: []slog.Attr{slog.String("agent", "sol")},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	logger.Debug("heartbeat sent", "request_id", "req-1", "err", "connection reset")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log record: %v\n%s", err, output.String())
	}
	for key, want := range map[string]string{
		"level":      "debug",
		"msg":        "heartbeat sent",
		"component":  "status-sidecar",
		"agent":      "sol",
		"request_id": "req-1",
		"error":      "connection reset",
	} {
		if got := record[key]; got != want {
			t.Errorf("record[%q] = %#v, want %q", key, got, want)
		}
	}
	timestamp, ok := record["time"].(string)
	if !ok {
		t.Fatalf("record[time] = %#v, want RFC3339 string", record["time"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		t.Fatalf("time %q is not RFC3339Nano: %v", timestamp, err)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("time location = %v, want UTC", parsed.Location())
	}
	if _, exists := record["err"]; exists {
		t.Errorf("record contains non-standard err field: %#v", record)
	}
}

func TestNewFiltersBelowConfiguredLevel(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(Config{Component: "control-plane", Level: "warn", Writer: &output})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	logger.Info("not emitted")
	logger.Warn("emitted")

	if strings.Contains(output.String(), "not emitted") {
		t.Errorf("output contains filtered record: %s", output.String())
	}
	if !strings.Contains(output.String(), `"msg":"emitted"`) {
		t.Errorf("output does not contain warning: %s", output.String())
	}
}

func TestNewRejectsMissingComponent(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() error = nil, want missing-component error")
	}
}
