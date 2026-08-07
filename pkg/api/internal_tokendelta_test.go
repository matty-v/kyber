package api

import (
	"testing"

	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// TestComputeTokenDelta pins the dual delta semantics:
//   - input/cache_creation/cache_read are quasi-cumulative — delta is the
//     monotone difference, with snap's full value on first report, model
//     change, or rollback (session reset);
//   - output is PER-MESSAGE spend and the in-pod reporter re-POSTs the same
//     last message every interval, so the delta is 0 only when the full token
//     tuple is unchanged against prev; any change means a new message and the
//     whole snap.Tokens.Output counts (never a subtraction).
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
			name: "first report (prev nil) — full snapshot including output",
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
			name: "new message, smaller output — full snap.Output, not a negative diff",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 500),
			snap: snap("claude-sonnet-4-6", 1200, 250, 80, 20),
			want: tokenstore.TokenDelta{Input: 200, CacheCreation: 50, CacheRead: 30, Output: 20},
		},
		{
			name: "new message, larger output — full snap.Output, not the diff",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 20),
			snap: snap("claude-sonnet-4-6", 1200, 200, 50, 500),
			want: tokenstore.TokenDelta{Input: 200, Output: 500},
		},
		{
			name: "output-only change (same context tuple) — new message, full output",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 20),
			snap: snap("claude-sonnet-4-6", 1000, 200, 50, 35),
			want: tokenstore.TokenDelta{Output: 35},
		},
		{
			name: "model change — full snapshot including output",
			prev: snap("claude-sonnet-4-6", 1000, 200, 50, 30),
			snap: snap("claude-opus-4-8", 400, 100, 20, 10),
			want: tokenstore.TokenDelta{Input: 400, CacheCreation: 100, CacheRead: 20, Output: 10},
		},
		{
			name: "input monotone increase unchanged by output semantics",
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
