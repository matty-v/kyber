// Package logging defines Kyber's process logging contract.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config describes one Kyber process logger.
type Config struct {
	// Component is the stable app.kubernetes.io/component value for the process.
	Component string
	// Level is debug, info, warn, or error. Empty defaults to info.
	Level string
	// Writer receives newline-delimited JSON. Nil defaults to stderr.
	Writer io.Writer
	// Attributes are stable process context added to every record.
	Attributes []slog.Attr
}

var processContextEnv = []struct {
	env string
	key string
}{
	{"KYBER_AGENT_NAME", "agent"},
	{"AGENT_NAME", "agent"},
	{"KYBER_MACHINE_NAME", "machine"},
	{"KYBER_LOG_POD", "pod"},
	{"KYBER_LOG_CONTAINER", "container"},
	{"KYBER_LOG_NAMESPACE", "namespace"},
}

// New constructs a JSON logger with Kyber's standard fields and level names.
func New(cfg Config) (*slog.Logger, error) {
	if strings.TrimSpace(cfg.Component) == "" {
		return nil, errors.New("logging: component is required")
	}

	level, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	writer := cfg.Writer
	if writer == nil {
		writer = os.Stderr
	}
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			switch attr.Key {
			case slog.TimeKey:
				return slog.Time(slog.TimeKey, attr.Value.Time().UTC())
			case slog.LevelKey:
				return slog.String(slog.LevelKey, strings.ToLower(attr.Value.String()))
			case "err":
				attr.Key = "error"
				return attr
			default:
				return attr
			}
		},
	})

	logger := slog.New(handler).With("component", cfg.Component)
	for _, field := range processContextEnv {
		if value := strings.TrimSpace(os.Getenv(field.env)); value != "" {
			logger = logger.With(field.key, value)
		}
	}
	if len(cfg.Attributes) > 0 {
		attrs := make([]any, 0, len(cfg.Attributes))
		for _, attr := range cfg.Attributes {
			attrs = append(attrs, attr)
		}
		logger = logger.With(attrs...)
	}
	return logger, nil
}

// ParseLevel validates a configured verbosity level. Empty input defaults to
// info so an omitted Helm value is quiet and backward compatible.
func ParseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: invalid level %q (want debug, info, warn, or error)", value)
	}
}
