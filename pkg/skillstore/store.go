// Package skillstore persists the most recent skill report each agent pushed.
//
// The report is produced in the pod by `kyber-skills report` (pkg/skillscan),
// forwarded through the status sidecar, and written here by
// POST /internal/agents/{name}/skills. The read-only
// GET /api/v1/agents/{name}/skills serves it back to the PWA.
//
// It is deliberately durable rather than a TTL cache. An agent reports at boot
// and on every identity sync, which can be days apart, so a store that expired
// between reports would blank the Skills tab and read as "this agent has no
// skills" — the same false-healthy shape the feature exists to eliminate.
package skillstore

import (
	"context"
	"errors"

	"github.com/matty-v/kyber/pkg/skillscan"
)

// ErrNotFound is returned by Get when the agent has never reported.
var ErrNotFound = errors.New("skillstore: no skill report for agent")

// errNilReport guards Put against a nil report, which would otherwise store a
// row that fails to decode on the way back out.
var errNilReport = errors.New("skillstore: Put called with a nil report")

// Store is the persistence contract. Implementations must be safe for
// concurrent use.
type Store interface {
	// Put replaces the stored report for agentName.
	Put(ctx context.Context, agentName string, report *skillscan.Report) error
	// Get returns the stored report, or ErrNotFound if the agent has never
	// reported.
	Get(ctx context.Context, agentName string) (*skillscan.Report, error)
	// Delete removes the agent's report. Deleting a missing agent is not an
	// error — the agent delete finalizer calls this unconditionally.
	Delete(ctx context.Context, agentName string) error
}
