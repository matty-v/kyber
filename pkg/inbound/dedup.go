package inbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// dedupeKeyPrefix is the Redis key prefix for the inbound dedupe set. Keeping
// it stable lets operators inspect the namespace under load.
const dedupeKeyPrefix = "inbound:dedupe:"

// Deduper records that a request body has been seen, returning whether it was
// already present within the supplied TTL. Implementations MUST be safe for
// concurrent use.
type Deduper interface {
	// SeenWithin returns true if the body hash was already recorded within the
	// TTL window, and atomically records it otherwise. SHA-256 of the request
	// body is the dedup key. Callers should treat any non-nil error as
	// fail-open (i.e. do not block the request) — but that policy lives in the
	// caller, not here.
	SeenWithin(ctx context.Context, body []byte, ttl time.Duration) (bool, error)
}

// hashBody returns the hex-encoded SHA-256 of body, used as the dedupe key
// suffix.
func hashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// RedisDeduper is a Deduper backed by go-redis using SET NX EX for atomic
// first-write-wins semantics.
type RedisDeduper struct {
	client *redis.Client
}

// NewRedisDeduper wraps a *redis.Client.
func NewRedisDeduper(client *redis.Client) *RedisDeduper {
	return &RedisDeduper{client: client}
}

// SeenWithin uses SETNX to record the body hash. SetNX returns true when the
// key was newly written (first sighting), so a true result from SetNX means
// SeenWithin returns false. A false result from SetNX means the key already
// existed and we treat the request as a duplicate.
func (d *RedisDeduper) SeenWithin(ctx context.Context, body []byte, ttl time.Duration) (bool, error) {
	key := dedupeKeyPrefix + hashBody(body)
	wasSet, err := d.client.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, err
	}
	return !wasSet, nil
}

// MemoryDeduper is an in-process Deduper. It is concurrency-safe and honors
// TTLs using time.Now(). Suitable for single-replica dev/test deployments
// where Redis isn't available — production should use RedisDeduper so the
// dedup window survives a control-plane restart.
type MemoryDeduper struct {
	mu      sync.Mutex
	entries map[string]time.Time // hash → expiry
	now     func() time.Time     // injectable for tests
}

// NewMemoryDeduper returns an empty in-memory Deduper.
func NewMemoryDeduper() *MemoryDeduper {
	return &MemoryDeduper{
		entries: make(map[string]time.Time),
		now:     time.Now,
	}
}

// newInMemoryDeduper retains the lower-cased name used by existing unit tests.
func newInMemoryDeduper() *MemoryDeduper {
	return NewMemoryDeduper()
}

// SeenWithin records the body hash with the given TTL, returning true if a
// non-expired entry already existed for the same body.
func (d *MemoryDeduper) SeenWithin(_ context.Context, body []byte, ttl time.Duration) (bool, error) {
	key := hashBody(body)
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if exp, ok := d.entries[key]; ok && exp.After(now) {
		return true, nil
	}
	d.entries[key] = now.Add(ttl)
	return false, nil
}
