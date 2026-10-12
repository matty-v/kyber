// Package metricsstore provides time-series and node-resource storage for the
// Kyber Metrics tab panels, backed by Redis sorted sets with a memory fallback
// for development installs without Redis.
//
// Key schema:
//
//	Time-series: ts:activity:{namespace}:{agent}:{state}  — sorted set, score=ts, member="{ts}:{value}"
//	Node latest: node:{namespace}:{node}                  — hash, fields cpuPercent/mem*/disk*/updatedAt
//
// Retention is applied by a background worker (retention.go) that runs
// ZREMRANGEBYSCORE every 5 minutes to evict points older than the configured
// horizon (default 7 days).
package metricsstore

import "context"

// Point is a single (timestamp, value) pair from a time series.
type Point struct {
	Timestamp int64
	Value     float64
}

// NodeSample holds the latest resource readings for one node.
type NodeSample struct {
	Node           string
	Namespace      string
	CPUPercent     float64
	MemUsedBytes   float64
	MemTotalBytes  float64
	DiskUsedBytes  float64
	DiskTotalBytes float64
	UpdatedAt      string
}

// MetricsStore persists time-series metric points and supports range queries.
// Implementations must be safe for concurrent use.
type MetricsStore interface {
	// AddPoint appends a (ts, value) point to the series identified by key.
	// key is pre-validated by the caller (e.g. "ts:activity:kyber-system:han:working").
	AddPoint(ctx context.Context, key string, ts int64, value float64) error

	// RangeQuery returns all points in [startTs, endTs] for the given key,
	// ordered by timestamp ascending. Returns an empty slice (not an error)
	// when no data exists.
	RangeQuery(ctx context.Context, key string, startTs, endTs int64) ([]Point, error)

	// DeleteAgent removes activity series and any legacy token series for
	// (namespace, agent).
	// Idempotent — no matching series is a no-op. The retention worker is only a
	// slow backstop, so the agent finalizer reaps these eagerly (kyber#565).
	DeleteAgent(ctx context.Context, namespace, agent string) error
}

// agentSeriesPrefixes includes the legacy token-series prefix so Agent deletion
// still reaps keys written before MAT-117. The trailing colon prevents prefix
// matching another Agent name.
func agentSeriesPrefixes(namespace, agent string) []string {
	return []string{
		"ts:activity:" + namespace + ":" + agent + ":",
		"ts:token_usage:" + namespace + ":" + agent + ":",
	}
}

// NodeStore persists the latest resource sample for each node (no time series).
// Implementations must be safe for concurrent use.
type NodeStore interface {
	// PutNode writes the latest sample for the given namespace + node.
	PutNode(ctx context.Context, namespace, node string, sample NodeSample) error

	// GetAllNodes returns the latest sample for every node in namespace.
	// Returns an empty slice (not an error) when no data exists.
	GetAllNodes(ctx context.Context, namespace string) ([]NodeSample, error)
}

// ActivityKey builds the Redis sorted-set key for an (agent, state) time series.
func ActivityKey(namespace, agent, state string) string {
	return "ts:activity:" + namespace + ":" + agent + ":" + state
}

// NodeKey builds the Redis hash key for a node's latest sample.
func NodeKey(namespace, node string) string {
	return "node:" + namespace + ":" + node
}
