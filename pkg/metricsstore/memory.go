package metricsstore

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// MemoryMetricsStore is an in-memory MetricsStore for development installs
// without Redis. Data evaporates on restart and is not shared across replicas.
// The control plane logs a WARN at boot when this store is selected.
type MemoryMetricsStore struct {
	mu     sync.RWMutex
	series map[string][]Point
}

// NewMemoryMetricsStore returns an empty MemoryMetricsStore.
func NewMemoryMetricsStore() *MemoryMetricsStore {
	return &MemoryMetricsStore{series: map[string][]Point{}}
}

// AddPoint appends a point and keeps the series sorted by timestamp.
func (m *MemoryMetricsStore) AddPoint(_ context.Context, key string, ts int64, value float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.series[key] = append(m.series[key], Point{Timestamp: ts, Value: value})
	pts := m.series[key]
	sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp < pts[j].Timestamp })
	m.series[key] = pts
	return nil
}

// RangeQuery returns points in [startTs, endTs] ordered by timestamp ascending.
func (m *MemoryMetricsStore) RangeQuery(_ context.Context, key string, startTs, endTs int64) ([]Point, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pts := m.series[key]
	var out []Point
	for _, p := range pts {
		if p.Timestamp >= startTs && p.Timestamp <= endTs {
			out = append(out, p)
		}
	}
	return out, nil
}

// EvictBefore removes points with Timestamp < olderThanTs from all series.
func (m *MemoryMetricsStore) EvictBefore(_ context.Context, olderThanTs int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, pts := range m.series {
		var keep []Point
		for _, p := range pts {
			if p.Timestamp >= olderThanTs {
				keep = append(keep, p)
			}
		}
		m.series[key] = keep
	}
	return nil
}

// DeleteAgent removes every time-series whose key falls under the per-agent
// ts:activity / ts:token_usage prefixes for (namespace, agent). Idempotent — no
// matching series is a no-op (kyber#565).
func (m *MemoryMetricsStore) DeleteAgent(_ context.Context, namespace, agent string) error {
	prefixes := agentSeriesPrefixes(namespace, agent)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.series {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				delete(m.series, key)
				break
			}
		}
	}
	return nil
}

// MemoryNodeStore is an in-memory NodeStore for development installs.
type MemoryNodeStore struct {
	mu    sync.RWMutex
	nodes map[string]NodeSample // key: namespace+":"+node
}

// NewMemoryNodeStore returns an empty MemoryNodeStore.
func NewMemoryNodeStore() *MemoryNodeStore {
	return &MemoryNodeStore{nodes: map[string]NodeSample{}}
}

// PutNode writes the latest sample for namespace/node.
func (m *MemoryNodeStore) PutNode(_ context.Context, namespace, node string, sample NodeSample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sample.Node = node
	sample.Namespace = namespace
	m.nodes[namespace+":"+node] = sample
	return nil
}

// GetAllNodes returns all samples for the given namespace.
func (m *MemoryNodeStore) GetAllNodes(_ context.Context, namespace string) ([]NodeSample, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []NodeSample
	prefix := namespace + ":"
	for k, s := range m.nodes {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, s)
		}
	}
	return out, nil
}
