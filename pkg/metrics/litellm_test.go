package metrics

import (
	"os"
	"testing"
)

// fixtureFeed mimics the shape of LiteLLM's
// model_prices_and_context_window.json: per-token costs keyed by model id,
// with a litellm_provider field. Values are per-token (× 1e6 → per-MTok).
const fixtureFeed = `{
  "claude-opus-4-8": {
    "input_cost_per_token": 5e-06,
    "output_cost_per_token": 2.5e-05,
    "cache_read_input_token_cost": 5e-07,
    "cache_creation_input_token_cost": 6.25e-06,
    "litellm_provider": "anthropic"
  },
  "gpt-4o": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05,
    "litellm_provider": "openai"
  },
  "command-r": {
    "input_cost_per_token": 5e-07,
    "output_cost_per_token": 1.5e-06,
    "litellm_provider": "cohere"
  },
  "garbled-model": {
    "input_cost_per_token": 1.0,
    "output_cost_per_token": 2.0,
    "litellm_provider": "anthropic"
  },
  "no-output-model": {
    "input_cost_per_token": 3e-06,
    "litellm_provider": "anthropic"
  },
  "zero-input-model": {
    "input_cost_per_token": 0,
    "output_cost_per_token": 1e-05,
    "litellm_provider": "openai"
  },
  "bad-cache-creation-model": {
    "input_cost_per_token": 3e-06,
    "output_cost_per_token": 1.5e-05,
    "cache_creation_input_token_cost": 1.0,
    "litellm_provider": "anthropic"
  }
}`

func projProviders() map[string]bool {
	return map[string]bool{"anthropic": true, "openai": true}
}

func projBounds() RateBounds {
	return RateBounds{MinPerMTok: 0, MaxPerMTok: 10_000}
}

// TestProjectLiteLLM_ConvertsAndFilters is the core adapter contract
// (kyber#487 v2): per-token feed costs project to kyber's per-MTok ProviderRates
// for the providers we surface; other providers are filtered out.
func TestProjectLiteLLM_ConvertsAndFilters(t *testing.T) {
	table, _, err := ProjectLiteLLM([]byte(fixtureFeed), projProviders(), projBounds())
	if err != nil {
		t.Fatalf("ProjectLiteLLM: %v", err)
	}

	opus, ok := table["claude-opus-4-8"]
	if !ok {
		t.Fatalf("claude-opus-4-8 missing from projection")
	}
	if opus.Input != 5.0 || opus.Output != 25.0 || opus.CacheRead != 0.5 || opus.CacheCreation != 6.25 {
		t.Errorf("opus projected = %+v, want {Input:5 Output:25 CacheRead:0.5 CacheCreation:6.25} per MTok", opus)
	}

	gpt, ok := table["gpt-4o"]
	if !ok {
		t.Fatalf("gpt-4o missing from projection")
	}
	if gpt.Input != 2.5 || gpt.Output != 10.0 || gpt.CacheRead != 0 || gpt.CacheCreation != 0 {
		t.Errorf("gpt-4o projected = %+v, want {2.5 10 0 0} (no cache_read/cache_creation → 0)", gpt)
	}

	if _, ok := table["command-r"]; ok {
		t.Errorf("command-r (cohere) must be filtered out — providers gate failed")
	}
}

// TestProjectLiteLLM_SanityBoundsReject ensures a malformed/poisoned upstream
// value (e.g. a per-token cost that wasn't scaled) is rejected rather than
// shipped as a confidently-wrong price — it falls to the unpriced badge.
func TestProjectLiteLLM_SanityBoundsReject(t *testing.T) {
	table, rejected, err := ProjectLiteLLM([]byte(fixtureFeed), projProviders(), projBounds())
	if err != nil {
		t.Fatalf("ProjectLiteLLM: %v", err)
	}
	// garbled-model: 1.0/token → 1,000,000/MTok > ceiling → rejected.
	if _, ok := table["garbled-model"]; ok {
		t.Errorf("garbled-model exceeded the sanity ceiling but was included")
	}
	// no-output-model: missing output cost → rejected (incomplete rate).
	if _, ok := table["no-output-model"]; ok {
		t.Errorf("no-output-model has no output cost but was included")
	}
	// zero-input-model: non-positive input → rejected.
	if _, ok := table["zero-input-model"]; ok {
		t.Errorf("zero-input-model has 0 input cost but was included")
	}
	// bad-cache-creation-model: unscaled cache_creation (1.0/token →
	// 1,000,000/MTok > ceiling) → the whole model is rejected, same as
	// cache_read.
	if _, ok := table["bad-cache-creation-model"]; ok {
		t.Errorf("bad-cache-creation-model exceeded the cache_creation sanity ceiling but was included")
	}
	if len(rejected) != 4 {
		t.Errorf("rejected count = %d, want 4 (garbled, no-output, zero-input, bad-cache-creation); got %v", len(rejected), rejected)
	}
}

// TestProjectLiteLLM_BadJSON surfaces a parse failure as an error so the
// fetcher/CI fails loud rather than emitting an empty dataset.
func TestProjectLiteLLM_BadJSON(t *testing.T) {
	if _, _, err := ProjectLiteLLM([]byte("not json"), projProviders(), projBounds()); err == nil {
		t.Errorf("expected error on malformed JSON, got nil")
	}
}

// TestRenderRatesYAML_RoundTrips ensures the vendored dataset the fetcher
// writes parses back to the same RateTable via the production loader — the
// build-time artifact and the runtime read path agree on the format.
func TestRenderRatesYAML_RoundTrips(t *testing.T) {
	table, _, err := ProjectLiteLLM([]byte(fixtureFeed), projProviders(), projBounds())
	if err != nil {
		t.Fatalf("ProjectLiteLLM: %v", err)
	}
	out, err := RenderRatesYAML(table, "# generated test header\n")
	if err != nil {
		t.Fatalf("RenderRatesYAML: %v", err)
	}
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Errorf("rendered YAML should be non-empty and newline-terminated")
	}

	dir := t.TempDir()
	path := dir + "/provider-rates.yaml"
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded := LoadRateTable(path)
	if len(loaded) != len(table) {
		t.Fatalf("round-trip size = %d, want %d", len(loaded), len(table))
	}
	for id, want := range table {
		got, ok := loaded[id]
		if !ok || got != want {
			t.Errorf("round-trip %s = %+v (ok=%v), want %+v", id, got, ok, want)
		}
	}
}
