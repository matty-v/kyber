// Package modelprobe classifies the outcome of the agent boot-time
// pre-flight model probe (`claude --model X --strict-mcp-config --print
// 'ping'`, start-claude.sh).
//
// The probe script used to classify its own stderr with a grep heuristic
// and reported only a tri-state bool. That failed open twice at once
// (kyber, canary regression run 2026-08-22): the Claude Code CLI prints
// its model-rejection message to STDOUT (which the probe discarded), and
// the current phrasing — "There's an issue with the selected model (X).
// It may not exist or you may not have access to it." — matched none of
// the grep patterns. A cluster whose fleet-default model was an invalid
// string showed healthy agents whose every turn failed.
//
// The script now reports the raw probe outcome (exit code + combined
// stdout/stderr, sanitized) and this package owns classification, so a
// future CLI rewording is a table update with a unit test instead of a
// silent regression inside a container image.
package modelprobe

import "regexp"

// Outcome is the classification of a single probe run.
type Outcome string

const (
	// OutcomeSupported: the CLI completed a one-shot turn on the model.
	OutcomeSupported Outcome = "supported"
	// OutcomeUnsupported: the CLI rejected the model itself.
	OutcomeUnsupported Outcome = "unsupported"
	// OutcomeInconclusive: the probe failed for a reason that is not a
	// model rejection (timeout, auth, network) — or for a reason we
	// could not recognize. Callers must surface this as "unknown", never
	// as "fine": collapsing it to silence is the exact failure mode this
	// package exists to close.
	OutcomeInconclusive Outcome = "inconclusive"
)

// timeoutExitCode is what `timeout(1)` returns on a hard timeout.
const timeoutExitCode = 124

// rejectionPatterns match model-rejection messages across the CLI
// phrasings observed so far. Case-insensitive. Deliberately anchored on
// the word "model" so auth/network errors ("Invalid bearer token",
// "connection refused") stay inconclusive.
var rejectionPatterns = []*regexp.Regexp{
	// Current phrasing (verified against claude CLI 2026-08-22):
	// "There's an issue with the selected model (X). It may not exist or
	// you may not have access to it."
	regexp.MustCompile(`(?i)issue with the selected model`),
	// Legacy phrasings the old script heuristic targeted: keyword and
	// "model" in either order.
	regexp.MustCompile(`(?i)(unsupported|invalid|unknown|not found|no such|does not (exist|support|recognize))[^a-z]+model`),
	regexp.MustCompile(`(?i)model[^a-z][^\n]{0,80}?(unsupported|invalid|unknown|not found|not available|may not exist|does not (exist|support|recognize))`),
}

// Classify maps a probe's exit code and combined stdout+stderr output to
// an Outcome. Exit 0 is authoritative success regardless of output (the
// CLI may still print deprecation warnings to stderr on success).
func Classify(exitCode int, output string) Outcome {
	if exitCode == 0 {
		return OutcomeSupported
	}
	if exitCode == timeoutExitCode {
		return OutcomeInconclusive
	}
	for _, p := range rejectionPatterns {
		if p.MatchString(output) {
			return OutcomeUnsupported
		}
	}
	return OutcomeInconclusive
}
