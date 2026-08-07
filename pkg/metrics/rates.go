package metrics

import (
	"math"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// datedSuffix matches a trailing Anthropic release-date suffix on a rate key,
// e.g. the "-20251001" in "claude-haiku-4-5-20251001". Claude Code emits the
// bare model ID (claude-haiku-4-5) in transcripts while the provider-rates
// table may key the same model by its dated release variant; the suffix is a
// publish detail, not a distinct pricing family (mirrors tokenreport.LimitFor's
// "[1m]" stripping). See kyber#487.
var datedSuffix = regexp.MustCompile(`-[0-9]{8}$`)

// ProviderRates holds per-token-type pricing for one model (USD per 1M tokens).
// An absent cache_creation key unmarshals to 0 (same semantics as cache_read):
// the component simply prices at $0 rather than making the model unpriced.
type ProviderRates struct {
	Input         float64 `yaml:"input"`
	Output        float64 `yaml:"output"`
	CacheRead     float64 `yaml:"cache_read"`
	CacheCreation float64 `yaml:"cache_creation"`
}

// RateTable maps model name → token-type rates.
type RateTable map[string]ProviderRates

// LoadRateTable reads provider-rates.yaml from path.
// Returns an empty table (not error) when the file is missing or unreadable —
// token costs will show as 0 rather than breaking the panel.
func LoadRateTable(path string) RateTable {
	if path == "" {
		return RateTable{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RateTable{}
	}
	var table RateTable
	if err := yaml.Unmarshal(data, &table); err != nil {
		return RateTable{}
	}
	return table
}

// ComputeCostKnown calculates the estimated USD cost for consumed tokens and
// reports whether the model was priced at all. tokenCounts maps token_type →
// count.
//
// The boolean is the fail-loud sentinel (kyber#487): it distinguishes
//   - (cost, true)  — a rate exists; cost may legitimately be 0 (zero usage)
//   - (0, false)    — no rate for this model; the caller must render "unpriced"
//     /"—" rather than a believable $0.0000.
//
// Lookup order: exact match first, then a date-normalized fallback that strips
// a trailing -YYYYMMDD release-date suffix from BOTH the looked-up id and the
// rate keys before comparing — so a bare id resolves a dated rate key
// (claude-haiku-4-5 → claude-haiku-4-5-20251001) AND a dated id resolves a bare
// rate key (claude-opus-4-8-20260601 → claude-opus-4-8). Exact match always
// wins; the fallback only fires on a miss. If two keys collapse to the same
// normalized id we keep the first deterministic match — not a current case.
func (rt RateTable) ComputeCostKnown(model string, tokenCounts map[string]float64) (float64, bool) {
	rates, ok := rt[model]
	if !ok {
		rates, ok = rt.normalizedLookup(model)
		if !ok {
			return 0, false
		}
	}
	cost := tokenCounts["input"]*rates.Input/1_000_000 +
		tokenCounts["output"]*rates.Output/1_000_000 +
		tokenCounts["cache_read"]*rates.CacheRead/1_000_000 +
		tokenCounts["cache_creation"]*rates.CacheCreation/1_000_000
	return math.Round(cost*10000) / 10000, true
}

// normalizedLookup resolves a model ID against rate keys with the trailing
// -YYYYMMDD release-date suffix stripped from both sides, so normalization
// works in either direction (bare↔dated). Only called after an exact-match
// miss; the identity (no-suffix on both) case is already covered by that exact
// match, so this only matters when at least one side carries a date.
func (rt RateTable) normalizedLookup(model string) (ProviderRates, bool) {
	want := datedSuffix.ReplaceAllString(model, "")
	for key, rates := range rt {
		if datedSuffix.ReplaceAllString(key, "") == want {
			return rates, true
		}
	}
	return ProviderRates{}, false
}

// ComputeCostUSD calculates the estimated USD cost for consumed tokens.
// tokenCounts maps token_type → count. Returns 0 when the model is unknown.
// Thin wrapper over ComputeCostKnown, retained for callers that don't need the
// priced sentinel.
func (rt RateTable) ComputeCostUSD(model string, tokenCounts map[string]float64) float64 {
	cost, _ := rt.ComputeCostKnown(model, tokenCounts)
	return cost
}
