package tokenreport

import "time"

// Snapshot is a single observation of an agent's context-budget state.
// It is the on-wire shape POSTed by the reporter and returned by the
// control plane's GET endpoint.
type Snapshot struct {
	Model       string    `json:"model"`
	Tokens      Tokens    `json:"tokens"`
	Percentage  float64   `json:"percentage"`
	EffortLevel string    `json:"effortLevel"`
	Speed       string    `json:"speed"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// ContextWindowKnown is false when the model's context window was not
	// found in the operator ConfigMap and the 200K floor was used (#396).
	// Set server-side at serve-time; the in-pod reporter leaves it false.
	// The PWA renders the % as an estimate ("≈") when this is false.
	ContextWindowKnown bool `json:"contextWindowKnown"`
}

// Tokens breaks down the usage numbers that make up the context window.
// Used = Input + CacheCreation + CacheRead.
type Tokens struct {
	Used          int64 `json:"used"`
	Limit         int64 `json:"limit"`
	Input         int64 `json:"input"`
	CacheCreation int64 `json:"cacheCreation"`
	CacheRead     int64 `json:"cacheRead"`
	// Output is the CUMULATIVE generated-token spend: since reporter start
	// for Claude Code (accumulated across finalized assistant messages by the
	// in-pod reporter's outputTracker) and since rollout-session start for
	// Codex (the rollout's total_token_usage counter). It is billed spend,
	// NOT part of the context-window formula — Used stays
	// Input + CacheCreation + CacheRead and must never include Output.
	// A reporter/rollout restart resets the counter; the control plane's
	// delta logic treats the rollback as a fresh increment (see
	// computeTokenDelta in pkg/api).
	Output int64 `json:"output"`
}
