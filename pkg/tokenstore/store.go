// Package tokenstore persists the most recent token-usage Snapshot per agent
// so the public API can serve GET /api/v1/agents/{name}/token-usage without
// round-tripping to the agent pod.
package tokenstore

import (
	"context"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

// TokenStore is the storage interface for per-agent token-usage snapshots.
// Implementations MUST apply a TTL (5 min by default) so missing agents
// return (nil, nil) rather than stale data.
type TokenStore interface {
	// Put writes the snapshot for agentName, resetting the TTL.
	Put(ctx context.Context, agentName string, snap *tokenreport.Snapshot) error

	// Get returns the snapshot for agentName, or (nil, nil) if absent or expired.
	// Errors are reserved for storage failures (Redis unreachable etc.).
	Get(ctx context.Context, agentName string) (*tokenreport.Snapshot, error)

	// Delete removes the snapshot for agentName. It is idempotent — deleting a
	// missing/expired key is a no-op (returns nil). Used by the agent finalizer
	// to reap per-agent token state on delete (kyber#565).
	Delete(ctx context.Context, agentName string) error
}
