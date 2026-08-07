package inbound

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// envelopeCacheTTL is how long a rendered envelope is retained for replay.
// 7 days matches the operator UX promise: "you can replay any inbound run
// from the last week." Anything older has aged out of the audit ring buffer
// too, so there's nothing useful for an operator to replay against.
const envelopeCacheTTL = 7 * 24 * time.Hour

// envelopeCacheKeyPrefix namespaces the cache keys inside Redis so operators
// inspecting the keyspace can tell envelopes apart from dedupe entries.
const envelopeCacheKeyPrefix = "inbound:envelope:"

// EnvelopeCache stores rendered inbound prompt envelopes keyed by request ID
// so a replay endpoint can re-fire them without re-walking the dispatcher
// pipeline. Implementations MUST be safe for concurrent use.
//
// Get returns ("", nil) when the key is unknown — callers distinguish
// "expired / never written" from real backend errors via the error return.
type EnvelopeCache interface {
	// Put stores envelope under requestID with the implementation's TTL.
	Put(ctx context.Context, requestID string, envelope string) error
	// Get returns the cached envelope for requestID. Returns ("", nil) when
	// the key is not present (expired or never written); a non-nil error
	// indicates a backend failure the caller should surface as 503.
	Get(ctx context.Context, requestID string) (string, error)
}

// RedisEnvelopeCache is the production EnvelopeCache backed by go-redis. The
// TTL is fixed at construction time but exposed as a struct field so tests
// can override it without monkey-patching the package constant.
type RedisEnvelopeCache struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedisEnvelopeCache wraps a *redis.Client with the default 7-day TTL.
func NewRedisEnvelopeCache(client *redis.Client) *RedisEnvelopeCache {
	return &RedisEnvelopeCache{client: client, ttl: envelopeCacheTTL}
}

// Put stores envelope under requestID using SET with the cache TTL. Last
// write wins — replays write a new request ID, so collisions on the
// original ID never occur in practice.
func (c *RedisEnvelopeCache) Put(ctx context.Context, requestID string, envelope string) error {
	key := envelopeCacheKeyPrefix + requestID
	return c.client.Set(ctx, key, envelope, c.ttl).Err()
}

// Get returns the cached envelope. A redis.Nil result is mapped to ("", nil)
// — that's the contract callers rely on to distinguish "expired" from
// "backend failure."
func (c *RedisEnvelopeCache) Get(ctx context.Context, requestID string) (string, error) {
	key := envelopeCacheKeyPrefix + requestID
	v, err := c.client.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// memoryEnvelopeEntry pairs the envelope text with its expiry time so the
// cache can lazily evict stale entries on read.
type memoryEnvelopeEntry struct {
	envelope string
	expiry   time.Time
}

// MemoryEnvelopeCache is the in-process EnvelopeCache for dev/test setups
// that don't have Redis. Single-replica only — entries do NOT survive a
// pod restart, so production should always use the Redis backend.
//
// Eviction is lazy: stale entries are not actively swept; they're dropped
// when a Get sees them past their expiry. The map grows in proportion to
// the number of distinct request IDs in the last TTL window — bounded in
// practice by the receiver's rate limits.
type MemoryEnvelopeCache struct {
	mu      sync.Mutex
	entries map[string]memoryEnvelopeEntry
	ttl     time.Duration
	now     func() time.Time // injectable for tests
}

// NewMemoryEnvelopeCache returns an empty in-memory EnvelopeCache.
func NewMemoryEnvelopeCache() *MemoryEnvelopeCache {
	return &MemoryEnvelopeCache{
		entries: make(map[string]memoryEnvelopeEntry),
		ttl:     envelopeCacheTTL,
		now:     time.Now,
	}
}

// Put stores envelope under requestID with the cache's TTL.
func (c *MemoryEnvelopeCache) Put(_ context.Context, requestID string, envelope string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[requestID] = memoryEnvelopeEntry{
		envelope: envelope,
		expiry:   c.now().Add(c.ttl),
	}
	return nil
}

// Get returns the cached envelope. Expired entries are evicted on read and
// reported as ("", nil) — the same shape callers expect for an unknown key.
func (c *MemoryEnvelopeCache) Get(_ context.Context, requestID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[requestID]
	if !ok {
		return "", nil
	}
	if !entry.expiry.After(c.now()) {
		delete(c.entries, requestID)
		return "", nil
	}
	return entry.envelope, nil
}
