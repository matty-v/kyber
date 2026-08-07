// Package runtimedetect polls the npm registry and the Anthropic Models API
// to surface newly-released Claude Code versions and Claude models without
// requiring a Kyber code change or rebuild. Results are cached (Redis in
// production, in-memory in dev/test) and read by GET /api/v1/available.
//
// See docs/design/2026-05-29-runtime-model-management-design.md §1 for design.
package runtimedetect

import "time"

// Snapshot is what the detection poller writes to the cache (Redis) and what
// the GET /api/v1/available handler returns to the PWA. The shape is the
// PR-A contract — PR-D extends Model.ContextWindow / ContextWindowKnown via
// the operator-editable override map.
type Snapshot struct {
	// ClaudeCodeVersions is the list of @anthropic-ai/claude-code versions
	// the poller observed on the npm registry, newest first. Capped at
	// VersionLimit entries to keep the picker sane (not full history).
	ClaudeCodeVersions []string `json:"claudeCodeVersions"`

	// CodexVersions is the list of @openai/codex versions observed on npm.
	CodexVersions []string `json:"codexVersions"`

	// Models is the list of Claude models the poller observed on the
	// Anthropic Models API, in the order the upstream returned them
	// (typically newest first by created_at). PR-A defaults every entry to
	// ContextWindow=DefaultContextWindowFloor + ContextWindowKnown=false;
	// PR-D layers the operator override map on top.
	Models []Model `json:"models"`

	// CodexModels is the latest picker-visible catalog reported by an
	// authenticated Codex agent via app-server model/list. Unlike the Claude
	// catalog, this reflects the user's ChatGPT subscription entitlements.
	CodexModels []Model `json:"codexModels"`

	// FetchedAt is when the poller wrote this snapshot. Surfaced for
	// debuggability — not part of the PWA contract.
	FetchedAt time.Time `json:"fetchedAt"`
}

// Model is a single Claude model entry in the Snapshot.
type Model struct {
	// ID is the model identifier sent to the Claude Code CLI
	// (e.g., "claude-opus-4-7").
	ID string `json:"id"`
	// DisplayName is the human-readable name from the Anthropic API
	// (e.g., "Claude Opus 4.7").
	DisplayName string `json:"displayName"`
	// ContextWindow is the total context-window size in tokens. The poller
	// adopts the Anthropic Models API's per-model max_input_tokens when
	// present (kyber#488); it falls back to DefaultContextWindowFloor (200K)
	// only when the field is absent. The operator-editable override map
	// (kyber-model-context-windows) still wins when it carries an entry for
	// the model — an optional edge-case override, no longer a required edit.
	ContextWindow int64 `json:"contextWindow"`
	// ContextWindowKnown is true when ContextWindow came from a confident
	// source (the API's max_input_tokens or an operator override) and false
	// when it is the floor default. The PWA renders an unknown window as an
	// estimate rather than a confident over-100% gauge.
	ContextWindowKnown bool `json:"contextWindowKnown"`
}

// DefaultContextWindowFloor is the safe floor PR-A applies to every model.
// 200K is the lowest context window any current Claude model ships with —
// under-reporting usage is recoverable, over-reporting is not.
const DefaultContextWindowFloor int64 = 200_000

// DefaultVersionLimit caps the number of CC versions surfaced through
// /available. The npm registry returns the full history; the PWA picker
// only needs a handful of recent options.
const DefaultVersionLimit = 20
