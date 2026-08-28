package main

import (
	"testing"
	"time"
)

func TestLoadAgentRequestConfigDefaultsDisabled(t *testing.T) {
	t.Setenv("KYBER_AGENT_REQUESTS_ENABLED", "")
	cfg, err := loadAgentRequestConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.Limits.Lifetime != time.Minute || cfg.Limits.MaxOutstanding != 2 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadAgentRequestConfigOverrides(t *testing.T) {
	t.Setenv("KYBER_AGENT_REQUESTS_ENABLED", "true")
	t.Setenv("KYBER_AGENT_REQUESTS_LIFETIME_SECONDS", "90")
	t.Setenv("KYBER_AGENT_REQUESTS_MAX_PROMPT_BYTES", "4096")
	t.Setenv("KYBER_AGENT_REQUESTS_MAX_CORRELATION_BYTES", "512")
	t.Setenv("KYBER_AGENT_REQUESTS_MAX_RESPONSE_BYTES", "16384")
	t.Setenv("KYBER_AGENT_REQUESTS_MAX_OUTSTANDING", "4")
	t.Setenv("KYBER_AGENT_REQUESTS_MAX_TERMINAL", "40")
	cfg, err := loadAgentRequestConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Limits.Lifetime != 90*time.Second || cfg.Limits.MaxPromptBytes != 4096 ||
		cfg.Limits.MaxCorrelationBytes != 512 || cfg.Limits.MaxResponseBytes != 16384 ||
		cfg.Limits.MaxOutstanding != 4 || cfg.Limits.MaxTerminal != 40 {
		t.Fatalf("unexpected overrides: %+v", cfg)
	}
}

func TestLoadAgentRequestConfigRejectsInvalid(t *testing.T) {
	t.Setenv("KYBER_AGENT_REQUESTS_MAX_OUTSTANDING", "9")
	if _, err := loadAgentRequestConfig(); err == nil {
		t.Fatal("expected hard-cap validation error")
	}
}
