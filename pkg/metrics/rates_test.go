package metrics

import "testing"

// TestComputeCostKnown_UnknownModel pins the fail-loud sentinel: an unknown
// model returns (0, false) so callers can distinguish "no rate for this model"
// from "rate present and the cost is genuinely 0" (kyber#487). The old
// ComputeCostUSD collapsed both to a believable $0.0000.
func TestComputeCostKnown_UnknownModel(t *testing.T) {
	rt := RateTable{
		"claude-sonnet-4-6": {Input: 3.0, Output: 15.0, CacheRead: 0.3},
	}
	cost, priced := rt.ComputeCostKnown("claude-opus-4-8", map[string]float64{"input": 1_000_000})
	if priced {
		t.Errorf("priced = true for unknown model, want false")
	}
	if cost != 0 {
		t.Errorf("cost = %v for unknown model, want 0", cost)
	}
}

// TestComputeCostKnown_KnownModelZeroUsage proves the distinction the badge
// relies on: a known model with zero usage is priced=true, cost=0 — genuinely
// $0, not "unpriced".
func TestComputeCostKnown_KnownModelZeroUsage(t *testing.T) {
	rt := RateTable{
		"claude-sonnet-4-6": {Input: 3.0, Output: 15.0, CacheRead: 0.3},
	}
	cost, priced := rt.ComputeCostKnown("claude-sonnet-4-6", map[string]float64{})
	if !priced {
		t.Errorf("priced = false for known model, want true")
	}
	if cost != 0 {
		t.Errorf("cost = %v for known model with zero usage, want 0", cost)
	}
}

// TestComputeCostKnown_KnownModelWithUsage confirms the cost math still works
// through the sentinel method.
func TestComputeCostKnown_KnownModelWithUsage(t *testing.T) {
	rt := RateTable{
		"claude-opus-4-8": {Input: 5.0, Output: 25.0, CacheRead: 0.5},
	}
	// 8159 input + 52898 cache_read against opus-4-8 verified rates ≈ lando's
	// sample from the issue (~$0.067), not $0.
	cost, priced := rt.ComputeCostKnown("claude-opus-4-8", map[string]float64{
		"input":      8159,
		"cache_read": 52898,
	})
	if !priced {
		t.Fatalf("priced = false, want true")
	}
	want := 8159*5.0/1_000_000 + 52898*0.5/1_000_000 // 0.0672...
	want = float64(int64(want*10000+0.5)) / 10000    // match ComputeCostUSD rounding
	if cost != want {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}

// TestComputeCostKnown_DatedKeyNormalization is the haiku fix (kyber#487
// design ruling): a rate key carrying a -YYYYMMDD release-date suffix
// (claude-haiku-4-5-20251001) must satisfy a bare-ID lookup
// (claude-haiku-4-5) so the model actually prices at runtime, not just in CI.
// A CI-only normalization would leave runtime mis-pricing to $0.
func TestComputeCostKnown_DatedKeyNormalization(t *testing.T) {
	rt := RateTable{
		"claude-haiku-4-5-20251001": {Input: 0.8, Output: 4.0, CacheRead: 0.08},
	}
	cost, priced := rt.ComputeCostKnown("claude-haiku-4-5", map[string]float64{"input": 1_000_000})
	if !priced {
		t.Fatalf("priced = false for dated-key model, want true (dated suffix must resolve the bare ID)")
	}
	if cost != 0.8 {
		t.Errorf("cost = %v, want 0.8 (1M input * $0.8/MTok)", cost)
	}
}

// TestComputeCostKnown_DatedLookupBareKey is the reverse normalization
// direction (kyber#487 v2): a dated runtime model id (claude-opus-4-8-20260601)
// resolves against a bare rate key (claude-opus-4-8). Normalization must work
// both ways so feed/runtime key shapes line up regardless of which side dates.
func TestComputeCostKnown_DatedLookupBareKey(t *testing.T) {
	rt := RateTable{
		"claude-opus-4-8": {Input: 5.0, Output: 25.0, CacheRead: 0.5},
	}
	cost, priced := rt.ComputeCostKnown("claude-opus-4-8-20260601", map[string]float64{"input": 1_000_000})
	if !priced {
		t.Fatalf("priced = false, want true (dated lookup must resolve a bare key)")
	}
	if cost != 5.0 {
		t.Errorf("cost = %v, want 5.0", cost)
	}
}

// TestComputeCostKnown_ExactMatchWins ensures exact match always beats the
// normalized fallback: if both a bare key and a dated key exist, the bare
// key's rate is used.
func TestComputeCostKnown_ExactMatchWins(t *testing.T) {
	rt := RateTable{
		"claude-haiku-4-5":          {Input: 1.0, Output: 4.0, CacheRead: 0.1},
		"claude-haiku-4-5-20251001": {Input: 99.0, Output: 99.0, CacheRead: 99.0},
	}
	cost, priced := rt.ComputeCostKnown("claude-haiku-4-5", map[string]float64{"input": 1_000_000})
	if !priced {
		t.Fatalf("priced = false, want true")
	}
	if cost != 1.0 {
		t.Errorf("cost = %v, want 1.0 (exact bare-key match must win over the dated key)", cost)
	}
}

// TestComputeCostUSD_WrapsComputeCostKnown pins the back-compat contract: the
// existing ComputeCostUSD keeps returning just the cost (0 for unknown) so
// nothing downstream of it breaks.
func TestComputeCostUSD_WrapsComputeCostKnown(t *testing.T) {
	rt := RateTable{
		"claude-sonnet-4-6": {Input: 3.0, Output: 15.0, CacheRead: 0.3},
	}
	if got := rt.ComputeCostUSD("claude-sonnet-4-6", map[string]float64{"input": 1_000_000}); got != 3.0 {
		t.Errorf("ComputeCostUSD known = %v, want 3.0", got)
	}
	if got := rt.ComputeCostUSD("nope", map[string]float64{"input": 1_000_000}); got != 0 {
		t.Errorf("ComputeCostUSD unknown = %v, want 0", got)
	}
}
