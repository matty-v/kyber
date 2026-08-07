package tokenstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

const (
	redisKeyPrefix = "token-usage:"
	defaultTTL     = 5 * time.Minute
)

// RedisStore is a Redis-backed TokenStore. Snapshots are JSON-encoded under
// the key "token-usage:{agent-name}" with a 5-minute TTL.
type RedisStore struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedisStore returns a RedisStore backed by the given client.
func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client, ttl: defaultTTL}
}

// Put writes snap under agentName with the configured TTL.
func (r *RedisStore) Put(ctx context.Context, agentName string, snap *tokenreport.Snapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}
	if err := r.client.Set(ctx, redisKeyPrefix+agentName, data, r.ttl).Err(); err != nil {
		return fmt.Errorf("setting redis key: %w", err)
	}
	return nil
}

// Get returns the snapshot for agentName, or (nil, nil) if absent/expired.
func (r *RedisStore) Get(ctx context.Context, agentName string) (*tokenreport.Snapshot, error) {
	raw, err := r.client.Get(ctx, redisKeyPrefix+agentName).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting redis key: %w", err)
	}
	var snap tokenreport.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("unmarshaling snapshot: %w", err)
	}
	return &snap, nil
}

// Delete removes the snapshot key for agentName. DEL of a missing key is a
// no-op in Redis, so this is naturally idempotent (kyber#565).
func (r *RedisStore) Delete(ctx context.Context, agentName string) error {
	if err := r.client.Del(ctx, redisKeyPrefix+agentName).Err(); err != nil {
		return fmt.Errorf("deleting redis key: %w", err)
	}
	return nil
}
