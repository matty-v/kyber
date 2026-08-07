package api

import (
	"context"
	"strings"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

// SnapshotWindowResolver resolves a model's auto-detected context window from
// the detection snapshot, returning (window, true) only when the snapshot
// carries a confident value for the model. Satisfied by
// *runtimedetect.SnapshotResolver (kyber#493). Defined here as an interface so
// the api package doesn't hard-depend on the concrete type and tests can
// inject a fake.
type SnapshotWindowResolver interface {
	LookupWindow(ctx context.Context, modelID string) (int64, bool)
}

// resolveContextWindow resolves the serve-time context-window limit + a
// "known" flag for a model id, using the SAME precedence settled in
// kyber#492/#493 and used by GET /api/v1/available and the pod adapter — so a
// given model resolves identically on every path (kyber#500):
//
//	1. operator override ConfigMap   (explicit human intent — top precedence)
//	2. detection snapshot            (auto-detected max_input_tokens)
//	3. tokenreport known table       (knownModels + opus/sonnet/haiku aliases)
//	4. 200K floor                    (unknown → contextWindowKnown=false)
//
// The "[1m]" opt-in suffix is stripped first (preserving the normalization the
// previous ContextWindows.LookupNormalized did), and the alias resolution is
// retained via tokenreport.LimitForKnown. nil-safe at every tier and never
// errors — the gauge serve path must always return 200, never 500, and must
// stay off any slow/unbounded cache call (the SnapshotResolver's TTL+timeout
// bounds guarantee that).
func (s *Server) resolveContextWindow(ctx context.Context, model string) (int64, bool) {
	base := strings.TrimSuffix(model, "[1m]")

	// 1. operator override ConfigMap — wins when the operator pinned the model.
	if s.ContextWindows != nil {
		if w, known := s.ContextWindows.LookupOr(ctx, base); known {
			return w, true
		}
	}
	// 2. detection snapshot — auto-detected window for models the poller saw.
	if s.Snapshots != nil {
		if w, known := s.Snapshots.LookupWindow(ctx, base); known {
			return w, true
		}
	}
	// 3 + 4. in-Go known table / family aliases, else the 200K floor (false).
	return tokenreport.LimitForKnown(base)
}
