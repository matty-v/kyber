// Package messagebuffer provides a short-lived buffer for messages sent to suspended agents.
//
// When an agent is Suspended, incoming messages (e.g., from a Telegram webhook) are
// buffered here so the controller can include them in the session brief when the agent
// wakes up. The Redis implementation enforces a 5-minute TTL; the in-memory
// implementation (for tests) has no TTL.
//
// The plan mentioned a Redis pub/sub subscriber goroutine. We defer that pattern —
// the API webhook (C1) handles CRD updates directly; the reconciler drains the Redis
// buffer during brief construction. See spec lines 361-381.
package messagebuffer

import (
	"context"

	"github.com/matty-v/kyber/pkg/briefstore"
)

// PendingMessage is an alias for briefstore.PendingMessage, re-exported here so
// callers of this package do not need to import briefstore just for the type.
// The canonical definition lives in pkg/briefstore/store.go.
type PendingMessage = briefstore.PendingMessage

// MessageBuffer buffers pending messages for suspended agents. Entries have a short
// TTL (5 minutes for Redis, no TTL for in-memory) and are drained atomically when
// the agent wakes up.
type MessageBuffer interface {
	// Push appends a message to the buffer for an agent. TTL is implementation-
	// specific (5 min for Redis, no TTL for in-memory).
	Push(ctx context.Context, agentName string, msg PendingMessage) error

	// Drain removes and returns all buffered messages for an agent. Returns an
	// empty slice (not an error) if there are no buffered messages.
	Drain(ctx context.Context, agentName string) ([]PendingMessage, error)

	// Delete discards any buffered messages for an agent without returning them.
	// Idempotent — no buffered messages is a no-op. Used by the agent finalizer
	// to reap the wake buffer on delete (kyber#565). The Redis buffer is TTL'd
	// (5 min), so this is mostly intent-clarity; the memory buffer needs it.
	Delete(ctx context.Context, agentName string) error
}
