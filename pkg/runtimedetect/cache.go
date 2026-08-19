package runtimedetect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache is what the poller writes to and what GET /api/v1/available reads
// from. A nil cache (or one returning ErrCacheEmpty) makes /available serve
// the empty fallback list — never crash.
type Cache interface {
	// Put stores snap, overwriting any previous value. TTL is set by the
	// implementation (Redis: enough margin over the poll cadence so a
	// missed poll still serves last-good).
	Put(ctx context.Context, snap *Snapshot) error
	// Get returns the most recent snapshot, or ErrCacheEmpty when nothing
	// has been written yet (or the Redis key has expired without a
	// replacement poll).
	Get(ctx context.Context) (*Snapshot, error)
}

// AgentCatalogCache stores authenticated model catalogs independently from the
// public harness snapshot, avoiding lost updates when agents and the npm poller
// report concurrently.
type AgentCatalogCache interface {
	PutAgentModels(ctx context.Context, agent string, models []Model) error
	GetAgentModels(ctx context.Context, agent string) ([]Model, error)
}

// ErrCacheEmpty signals that no snapshot has been written, or the cached
// snapshot has expired. /available returns the empty fallback in this case.
var ErrCacheEmpty = errors.New("runtimedetect: cache empty")

// MemoryCache is the in-memory Cache implementation used when Redis is not
// configured (single-replica dev/test). Multi-replica installs MUST use the
// Redis cache — a per-replica memory cache would make /available answer
// differently on each replica and flicker the PWA picker.
type MemoryCache struct {
	mu          sync.RWMutex
	snap        *Snapshot
	agentModels map[string][]Model
}

// NewMemoryCache returns an empty MemoryCache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{}
}

// Put stores snap. The caller owns snap after Put — we keep a deep copy via
// the JSON round-trip the production Redis path uses anyway, so memory and
// Redis behave identically for tests.
func (m *MemoryCache) Put(ctx context.Context, snap *Snapshot) error {
	if snap == nil {
		return errors.New("runtimedetect: Put nil snapshot")
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}
	var copy Snapshot
	if err := json.Unmarshal(data, &copy); err != nil {
		return fmt.Errorf("round-tripping snapshot: %w", err)
	}
	m.mu.Lock()
	m.snap = &copy
	m.mu.Unlock()
	return nil
}

// Get returns the most recent snapshot, or ErrCacheEmpty.
func (m *MemoryCache) Get(ctx context.Context) (*Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.snap == nil {
		return nil, ErrCacheEmpty
	}
	// Return a defensive copy — callers writing to the returned snap must
	// not mutate the cached value.
	var copy Snapshot
	data, err := json.Marshal(m.snap)
	if err != nil {
		return nil, fmt.Errorf("marshaling cached snapshot: %w", err)
	}
	if err := json.Unmarshal(data, &copy); err != nil {
		return nil, fmt.Errorf("unmarshaling cached snapshot: %w", err)
	}
	return &copy, nil
}

func (m *MemoryCache) PutAgentModels(_ context.Context, agent string, models []Model) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.agentModels == nil {
		m.agentModels = make(map[string][]Model)
	}
	m.agentModels[agent] = append([]Model(nil), models...)
	return nil
}

func (m *MemoryCache) GetAgentModels(_ context.Context, agent string) ([]Model, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.agentModels[agent]) == 0 {
		return nil, ErrCacheEmpty
	}
	return append([]Model(nil), m.agentModels[agent]...), nil
}

// redisKey is where the snapshot lives in Redis. Single key — multi-replica
// installs hit the same blob so /available is consistent across replicas.
const redisKey = "runtimedetect:available"
const redisAgentModelsKey = "runtimedetect:agent-models"

// defaultRedisTTL gives the cache enough margin over the poll cadence that
// a single missed poll still serves last-good. The poller refreshes every
// CadenceSeconds; we let the cache survive 4 missed cycles by default
// (max(cadence*4, 1h) is applied by NewRedisCache).
const defaultRedisTTL = 24 * time.Hour

// RedisCache is the Redis-backed Cache. Multi-replica installs use this so
// every replica's /available returns the same blob.
type RedisCache struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedisCache returns a Cache backed by client with the given TTL. If ttl
// is <=0, defaultRedisTTL is used.
func NewRedisCache(client *redis.Client, ttl time.Duration) *RedisCache {
	if ttl <= 0 {
		ttl = defaultRedisTTL
	}
	return &RedisCache{client: client, ttl: ttl}
}

// Put writes snap to Redis as JSON with the configured TTL.
func (r *RedisCache) Put(ctx context.Context, snap *Snapshot) error {
	if snap == nil {
		return errors.New("runtimedetect: Put nil snapshot")
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}
	if err := r.client.Set(ctx, redisKey, data, r.ttl).Err(); err != nil {
		return fmt.Errorf("setting redis key: %w", err)
	}
	return nil
}

// Get returns the cached snapshot, or ErrCacheEmpty when the key is absent
// or expired.
func (r *RedisCache) Get(ctx context.Context) (*Snapshot, error) {
	raw, err := r.client.Get(ctx, redisKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrCacheEmpty
	}
	if err != nil {
		return nil, fmt.Errorf("getting redis key: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("unmarshaling snapshot: %w", err)
	}
	return &snap, nil
}

func (r *RedisCache) PutAgentModels(ctx context.Context, agent string, models []Model) error {
	data, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("marshaling agent models: %w", err)
	}
	pipe := r.client.TxPipeline()
	pipe.HSet(ctx, redisAgentModelsKey, agent, data)
	pipe.Expire(ctx, redisAgentModelsKey, r.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("setting agent models: %w", err)
	}
	return nil
}

func (r *RedisCache) GetAgentModels(ctx context.Context, agent string) ([]Model, error) {
	raw, err := r.client.HGet(ctx, redisAgentModelsKey, agent).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrCacheEmpty
	}
	if err != nil {
		return nil, fmt.Errorf("getting agent models: %w", err)
	}
	var models []Model
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, fmt.Errorf("unmarshaling agent models: %w", err)
	}
	return models, nil
}

// Compile-time assertion: both implementations satisfy the Cache interface.
var (
	_ Cache             = (*MemoryCache)(nil)
	_ Cache             = (*RedisCache)(nil)
	_ AgentCatalogCache = (*MemoryCache)(nil)
	_ AgentCatalogCache = (*RedisCache)(nil)
)
