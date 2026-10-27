// Package reasoningeffort contains the versioned native-client evidence used
// before Kyber accepts an agent-wide effort override. It is not a provider
// entitlement catalog or a guarantee of model reasoning quality.
package reasoningeffort

import (
	"fmt"
	"slices"
)

// Options describes the tested native-client control for this exact tuple.
// An empty level list never means a fixed default; Default removes our override.
type Options struct {
	State  string   `json:"state"`
	Levels []string `json:"levels"`
	Source string   `json:"source,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// Lookup uses exact client versions. Evidence comes from initialize/model-list
// responses collected without inference on 2026-10-26. New versions, aliases and
// external inference routes remain unknown until separately qualified.
func Lookup(runtime, version, model string, customEndpoint bool) Options {
	result := Options{State: "unknown", Levels: []string{}, Reason: "Effort support has not been verified for this combination."}
	if runtime != "codex" && runtime != "claude-code" {
		result.State = "unsupported"
		result.Reason = "This runtime does not support Kyber effort settings."
		return result
	}
	if customEndpoint {
		return result
	}
	var levels []string
	if runtime == "claude-code" && version == "2.1.119" {
		switch model {
		case "claude-opus-4-7", "opus", "opus[1m]":
			levels = []string{"low", "medium", "high", "xhigh", "max"}
		case "claude-sonnet-4-6", "sonnet", "sonnet[1m]":
			levels = []string{"low", "medium", "high", "max"}
		}
		result.Source = "claude-sdk-initialize/2.1.119"
	}
	if runtime == "codex" && version == "0.146.0" {
		switch model {
		case "gpt-5.6-sol", "gpt-5.6-terra":
			levels = []string{"low", "medium", "high", "xhigh", "max", "ultra"}
		case "gpt-5.6-luna":
			levels = []string{"low", "medium", "high", "xhigh", "max"}
		case "gpt-5.5", "gpt-5.2":
			levels = []string{"low", "medium", "high", "xhigh"}
		}
		result.Source = "codex-model-list/0.146.0"
	}
	if len(levels) > 0 {
		result.State = "supported"
		result.Levels = levels
		result.Reason = ""
	} else {
		result.Source = ""
	}
	return result
}

// Validate never requires evidence to remove a Kyber override.
func Validate(runtime, version, model string, customEndpoint bool, effort string) error {
	if effort == "" {
		return nil
	}
	options := Lookup(runtime, version, model, customEndpoint)
	if options.State != "supported" || !slices.Contains(options.Levels, effort) {
		return fmt.Errorf("reasoning effort is not supported for this runtime version, model and route; choose a supported level or clear effort first")
	}
	return nil
}
