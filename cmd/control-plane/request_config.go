package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/matty-v/kyber/pkg/requeststore"
)

type agentRequestConfig struct {
	Limits requeststore.Limits
}

func loadAgentRequestConfig() (agentRequestConfig, error) {
	cfg := agentRequestConfig{
		Limits: requeststore.DefaultLimits(),
	}
	fields := []struct {
		name string
		dst  *int
	}{
		{"KYBER_AGENT_REQUESTS_LIFETIME_SECONDS", nil},
		{"KYBER_AGENT_REQUESTS_MAX_PROMPT_BYTES", &cfg.Limits.MaxPromptBytes},
		{"KYBER_AGENT_REQUESTS_MAX_CORRELATION_BYTES", &cfg.Limits.MaxCorrelationBytes},
		{"KYBER_AGENT_REQUESTS_MAX_RESPONSE_BYTES", &cfg.Limits.MaxResponseBytes},
		{"KYBER_AGENT_REQUESTS_MAX_OUTSTANDING", &cfg.Limits.MaxOutstanding},
		{"KYBER_AGENT_REQUESTS_MAX_TERMINAL", &cfg.Limits.MaxTerminal},
	}
	for _, field := range fields {
		raw := os.Getenv(field.name)
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return agentRequestConfig{}, fmt.Errorf("%s must be a positive integer", field.name)
		}
		if field.dst == nil {
			cfg.Limits.Lifetime = time.Duration(value) * time.Second
		} else {
			*field.dst = value
		}
	}
	if err := cfg.Limits.Validate(); err != nil {
		return agentRequestConfig{}, fmt.Errorf("agent request limits: %w", err)
	}
	return cfg, nil
}
