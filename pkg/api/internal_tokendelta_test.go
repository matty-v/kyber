package api

import (
	"testing"

	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// TestComputeTokenDelta pins the delta semantics shared by all four token
// types: every type is a quasi-cumulative counter (output is the
// reporter-accumulated total since reporter/rollout start) flowing through
// safeDelta — growing counter → the diff; first report / model change → the
// full snapshot value; rollback (reporter or session restart) → the new
// smaller value as a fresh increment; and never a negative delta, so a
// malformed negative count in a reporter POST clamps to 0.
func TestComputeTokenDelta(t *testing.T) {
	snap := func(model string, input, cacheCreation, cacheRead, output int64) *tokenreport.Snapshot {
		return &tokenreport.Snapshot{
			Model: model,
			Tokens: tokenreport.Tokens{
				Input:         input,
				CacheCreation: cacheCreation,
				CacheRead:     cacheRead,
				Output:        output,
			},
		}
	}

	tests := []struct {
		name string
		prev *tokenreport.Snapshot
		snap *tokenreport.Snapshot
		want tokenstore.TokenDelta
	}{
		{
			name: "first report (prev nil, e.g. store TTL expired) — full snapshot including output",
			prev: nil,
			snap: snap("claude-sonnet-4-6", 1000, 200, 50, 30),
			want: tokenstore.TokenDelta{Input: 1000, CacheCreation: 200, CacheRead: 50, Output: 30},
		},
		{
			name: "identical snapshot re-reported — all-zero delta including output",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 30),
			snap: snap("claude-sonnet-4-6", 1000, 200, 50, 30),
			want: tokenstore.TokenDelta{},
		},
		{
			name: "growing cumulative output — delta is the diff",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 100),
			snap: snap("claude-sonnet-4-6", 1200, 250, 80, 130),
			want: tokenstore.TokenDelta{Input: 200, CacheCreation: 50, CacheRead: 30, Output: 30},
		},
		{
			name: "output-only growth (context tuple unchanged) — diff, not full value",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 100),
			snap: snap("claude-sonnet-4-6", 1000, 200, 50, 145),
			want: tokenstore.TokenDelta{Output: 45},
		},
		{
			name: "output rollback (reporter restart) — full new value as a fresh increment",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 5000),
			snap: snap("claude-sonnet-4-6", 1200, 200, 50, 40),
			want: tokenstore.TokenDelta{Input: 200, Output: 40},
		},
		{
			name: "model change — full snapshot including output",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 30),
			snap: snap("claude-opus-4-8", 400, 100, 20, 10),
			want: tokenstore.TokenDelta{Input: 400, CacheCreation: 100, CacheRead: 20, Output: 10},
		},
		{
			name: "input monotone increase unchanged",
			prev: snap("claude-sonnet-4-6", 1000, 0, 0, 0),
			snap: snap("claude-sonnet-4-6", 1500, 0, 0, 0),
			want: tokenstore.TokenDelta{Input: 500},
		},
		{
			name: "input rollback (session reset) — snap value is the increment",
			prev: snap("claude-sonnet-4-6", 1500, 0, 0, 0),
			snap: snap("claude-sonnet-4-6", 300, 0, 0, 12),
			want: tokenstore.TokenDelta{Input: 300, Output: 12},
		},
		{
			name: "negative output in a malformed POST — clamped to 0, never negative",
			prev: snap("claude-sonnet-4-6", 1000, 0, 0, 50),
			snap: snap("claude-sonnet-4-6", 1000, 0, 0, -20),
			want: tokenstore.TokenDelta{},
		},
		{
			name: "negative output with no prev — still 0",
			prev: nil,
			snap: snap("claude-sonnet-4-6", 0, 0, 0, -5),
			want: tokenstore.TokenDelta{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTokenDelta(tt.prev, tt.snap)
			if got != tt.want {
				t.Errorf("computeTokenDelta() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
