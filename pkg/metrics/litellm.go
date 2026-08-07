package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"gopkg.in/yaml.v3"
)

// litellmEntry is the subset of a LiteLLM
// model_prices_and_context_window.json entry kyber consumes. Costs are
// per-token; pointers distinguish "absent" from a genuine 0. Unknown fields are
// ignored (the upstream entry carries dozens of capability flags we don't use).
type litellmEntry struct {
	InputCostPerToken       *float64 `json:"input_cost_per_token"`
	OutputCostPerToken      *float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost *float64 `json:"cache_read_input_token_cost"`
	LiteLLMProvider         string   `json:"litellm_provider"`
}

// RateBounds gates adopted per-MTok rates. A value must satisfy
// MinPerMTok < rate <= MaxPerMTok; anything outside is treated as a malformed
// or poisoned upstream value and the model is rejected (→ unpriced badge)
// rather than shipped as a confidently-wrong price (kyber#487 v2).
type RateBounds struct {
	MinPerMTok float64
	MaxPerMTok float64
}

const perMTok = 1_000_000.0

// ProjectLiteLLM projects a LiteLLM model-prices JSON document into kyber's
// per-MTok RateTable, keeping only entries whose litellm_provider is in
// providers and whose input/output rates pass bounds. It returns the projected
// table, the sorted ids of models rejected by the bounds/completeness checks
// (for visibility), and an error only on a JSON parse failure.
//
// This is the build-time adapter for the vendored-pin architecture: the third
// party never enters kyber's runtime path — its data is converted here, written
// to a reviewed vendored dataset, and rendered into the existing ConfigMap.
func ProjectLiteLLM(raw []byte, providers map[string]bool, bounds RateBounds) (RateTable, []string, error) {
	var feed map[string]litellmEntry
	if err := json.Unmarshal(raw, &feed); err != nil {
		return nil, nil, fmt.Errorf("parse litellm feed: %w", err)
	}

	table := RateTable{}
	var rejected []string
	for id, e := range feed {
		if !providers[e.LiteLLMProvider] {
			continue
		}
		// input and output are required and must pass bounds.
		if e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
			rejected = append(rejected, id)
			continue
		}
		input := round4(*e.InputCostPerToken * perMTok)
		output := round4(*e.OutputCostPerToken * perMTok)
		if !bounds.ok(input) || !bounds.ok(output) {
			rejected = append(rejected, id)
			continue
		}
		// cache_read is optional; default 0, but if present it must be a sane
		// non-negative value within the ceiling.
		var cacheRead float64
		if e.CacheReadInputTokenCost != nil {
			cacheRead = round4(*e.CacheReadInputTokenCost * perMTok)
			if cacheRead < 0 || cacheRead > bounds.MaxPerMTok {
				rejected = append(rejected, id)
				continue
			}
		}
		table[id] = ProviderRates{Input: input, Output: output, CacheRead: cacheRead}
	}
	sort.Strings(rejected)
	return table, rejected, nil
}

// ok reports whether a per-MTok rate is within (Min, Max].
func (b RateBounds) ok(rate float64) bool {
	return rate > b.MinPerMTok && rate <= b.MaxPerMTok
}

// RenderRatesYAML marshals a RateTable to the provider-rates.yaml format the
// chart vendors and LoadRateTable reads, prefixed with the given header
// (attribution / generation stamp as YAML comments). yaml.v3 emits map keys
// sorted, so the output is deterministic across runs — a clean diff for the
// refresh-bot PR.
func RenderRatesYAML(table RateTable, header string) ([]byte, error) {
	body, err := yaml.Marshal(table)
	if err != nil {
		return nil, fmt.Errorf("marshal rates: %w", err)
	}
	out := append([]byte(header), body...)
	if len(out) == 0 || out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, nil
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}
