// Package tokenreport parses Claude Code JSONL transcripts and reports
// context-window usage to the Kyber control plane.
package tokenreport

import (
	"sort"
	"strings"
)

// Model describes a Claude model Kyber knows about — used both internally to
// size the context window when parsing transcripts, and surfaced by the
// control-plane config endpoint so the PWA can populate the Create-Agent
// dropdown from a single source of truth.
type Model struct {
	// ID is the model identifier as sent to Claude Code (and as emitted in
	// the JSONL transcripts). Example: "claude-opus-4-7".
	ID string `json:"id"`
	// ContextWindow is the total context-window size in tokens.
	ContextWindow int64 `json:"contextWindow"`
}

// knownModels lists every concrete Claude model Kyber is aware of, in the
// order agents should see them in UI dropdowns (newest generation first,
// then opus → sonnet → haiku within a generation). Add an entry here when a
// new model ships — the control-plane config endpoint + the PWA pick it up
// automatically on the next rebuild.
var knownModels = []Model{
	{ID: "claude-opus-4-7", ContextWindow: 1_000_000},
	{ID: "claude-opus-4-6", ContextWindow: 1_000_000},
	{ID: "claude-opus-4-5", ContextWindow: 200_000},
	{ID: "claude-opus-4", ContextWindow: 200_000},
	{ID: "claude-sonnet-4-6", ContextWindow: 1_000_000},
	{ID: "claude-sonnet-4-5", ContextWindow: 200_000},
	{ID: "claude-sonnet-4", ContextWindow: 200_000},
	{ID: "claude-haiku-4-5", ContextWindow: 200_000},
}

// aliasLimits maps short aliases Claude Code may emit in its JSONL (e.g.
// "sonnet", "opus") to their context-window size. These aren't surfaced in
// the UI — they only exist so transcript parsing gets the right number when
// an agent ran with a family-alias --model flag.
var aliasLimits = map[string]int64{
	"opus":   1_000_000,
	"sonnet": 200_000,
	"haiku":  200_000,
}

// LimitFor returns the context-window token limit for the given model
// identifier (concrete ID or alias). Unknown models return the safe default
// of 200_000.
//
// Strips the "[1m]" opt-in suffix before lookup. Claude Code emits the
// suffix into its JSONL transcripts when the 1M-context variant is in use
// (e.g. "claude-opus-4-7[1m]") — the suffix is an invocation detail, not a
// distinct model family. The table's listed ContextWindow already reflects
// the 1M variant for models that support it, so the base-name match is the
// right answer whether the suffix is present or not.
func LimitFor(model string) int64 {
	limit, _ := LimitForKnown(model)
	return limit
}

// LimitForKnown is LimitFor with an explicit "known" signal: it returns
// (window, true) when the model resolves from the knownModels table or a
// family alias, and (200_000, false) when it falls through to the floor.
// Callers that surface a contextWindowKnown flag (the serve-time token-budget
// gauge, kyber#500) need to distinguish a confidently-known 200K model from
// the unknown-model floor — LimitFor's bare int can't. Same "[1m]"-suffix
// strip + alias resolution as LimitFor.
func LimitForKnown(model string) (int64, bool) {
	base := strings.TrimSuffix(model, "[1m]")
	for i := range knownModels {
		if knownModels[i].ID == base {
			return knownModels[i].ContextWindow, true
		}
	}
	if v, ok := aliasLimits[base]; ok {
		return v, true
	}
	return 200_000, false
}

// Models returns the concrete models Kyber knows about, suitable for the
// public config endpoint. The returned slice is a copy — callers can modify
// it without affecting the internal table. Aliases like "opus" are excluded.
func Models() []Model {
	out := make([]Model, len(knownModels))
	copy(out, knownModels)
	// Sort kept deterministic for tests; the intent-ordered knownModels
	// above already gives a reasonable UI ordering, so we re-sort only by a
	// stable rule (family then numeric suffix descending).
	sort.SliceStable(out, func(i, j int) bool {
		return modelOrder(out[i].ID) < modelOrder(out[j].ID)
	})
	return out
}

// modelOrder assigns a stable sort rank: lower is earlier in the UI.
// Newest opus → oldest opus → sonnet → haiku, keeping intent-ordered groups.
func modelOrder(id string) int {
	for i, m := range knownModels {
		if m.ID == id {
			return i
		}
	}
	return len(knownModels) // unknown ids sort last
}
