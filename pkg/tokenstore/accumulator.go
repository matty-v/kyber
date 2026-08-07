package tokenstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

const accumKeyPrefix = "accum:token_usage:"

// TokenDelta holds the per-type token increment from one snapshot to the next.
type TokenDelta struct {
	Input         int64
	CacheCreation int64
	CacheRead     int64
	Output        int64
}

// IsZero reports whether all token counts in the delta are zero.
func (d TokenDelta) IsZero() bool {
	return d.Input == 0 && d.CacheCreation == 0 && d.CacheRead == 0 && d.Output == 0
}

// AgentModelCounts holds accumulated token counts for one (agent, model) pair.
type AgentModelCounts struct {
	Agent         string
	Model         string
	Input         int64
	CacheCreation int64
	CacheRead     int64
	Output        int64
}

// Accumulator accumulates per-agent/model token counts so the Token Usage
// metrics panel is always available without a Prometheus TSDB dependency.
type Accumulator interface {
	// IncrBy increments the token counts for (namespace, agentName, model) by delta.
	// A zero delta is a no-op.
	IncrBy(ctx context.Context, namespace, agentName, model string, delta TokenDelta) error
	// GetAll returns accumulated counts for all (agent, model) pairs in namespace.
	// Returns an empty slice (not an error) when no data exists.
	GetAll(ctx context.Context, namespace string) ([]AgentModelCounts, error)
	// DeleteAgent removes all accumulated counts for (namespace, agentName)
	// across every model. Idempotent — no rows is a no-op. These hashes have no
	// TTL, so the agent finalizer must delete them explicitly (kyber#565).
	DeleteAgent(ctx context.Context, namespace, agentName string) error
}

// RedisAccumulator is a Redis-backed Accumulator. Each (namespace, agent, model)
// triple is stored as a Redis hash at key accum:token_usage:{namespace}:{agent}:{model}
// with fields input, cache_creation, cache_read, output. Counts are incremented with HINCRBY
// so concurrent writers are safe without application-level locking.
// No TTL is applied — counts are permanent and accumulate across agent restarts.
type RedisAccumulator struct {
	client *redis.Client
}

// NewRedisAccumulator returns a RedisAccumulator backed by client.
func NewRedisAccumulator(client *redis.Client) *RedisAccumulator {
	return &RedisAccumulator{client: client}
}

// IncrBy atomically increments the hash fields for (namespace, agent, model).
func (a *RedisAccumulator) IncrBy(ctx context.Context, namespace, agentName, model string, delta TokenDelta) error {
	if delta.IsZero() {
		return nil
	}
	key := accumKey(namespace, agentName, model)
	pipe := a.client.TxPipeline()
	if delta.Input > 0 {
		pipe.HIncrBy(ctx, key, "input", delta.Input)
	}
	if delta.CacheCreation > 0 {
		pipe.HIncrBy(ctx, key, "cache_creation", delta.CacheCreation)
	}
	if delta.CacheRead > 0 {
		pipe.HIncrBy(ctx, key, "cache_read", delta.CacheRead)
	}
	if delta.Output > 0 {
		pipe.HIncrBy(ctx, key, "output", delta.Output)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// GetAll scans for all keys matching accum:token_usage:{namespace}:* and returns
// the accumulated counts for each (agent, model) pair found.
func (a *RedisAccumulator) GetAll(ctx context.Context, namespace string) ([]AgentModelCounts, error) {
	pattern := accumKeyPrefix + namespace + ":*"
	nsPrefix := accumKeyPrefix + namespace + ":"

	var keys []string
	iter := a.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scanning accumulator keys: %w", err)
	}

	out := make([]AgentModelCounts, 0, len(keys))
	for _, key := range keys {
		agent, model, ok := parseAccumKeySuffix(strings.TrimPrefix(key, nsPrefix))
		if !ok {
			continue
		}
		fields, err := a.client.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("reading accumulator key %q: %w", key, err)
		}
		counts := AgentModelCounts{Agent: agent, Model: model}
		counts.Input, _ = strconv.ParseInt(fields["input"], 10, 64)
		counts.CacheCreation, _ = strconv.ParseInt(fields["cache_creation"], 10, 64)
		counts.CacheRead, _ = strconv.ParseInt(fields["cache_read"], 10, 64)
		// The output field is intentionally read as 0 when absent: hashes
		// written before the output field existed simply lack it, and ParseInt
		// of the missing-key empty string yields 0 — the correct back-compat
		// value for a pre-upgrade hash.
		counts.Output, _ = strconv.ParseInt(fields["output"], 10, 64)
		out = append(out, counts)
	}
	return out, nil
}

// DeleteAgent scans for all keys matching accum:token_usage:{namespace}:{agent}:*
// (one per model) and deletes them. Mirrors the GetAll scan pattern; DEL of a
// missing key is a no-op, so this is idempotent (kyber#565).
func (a *RedisAccumulator) DeleteAgent(ctx context.Context, namespace, agentName string) error {
	pattern := accumKeyPrefix + namespace + ":" + agentName + ":*"
	iter := a.client.Scan(ctx, 0, pattern, 0).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("scanning accumulator keys for delete: %w", err)
	}
	if len(keys) == 0 {
		return nil
	}
	if err := a.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("deleting accumulator keys: %w", err)
	}
	return nil
}

// accumKey builds the Redis hash key for the given (namespace, agent, model) triple.
func accumKey(namespace, agentName, model string) string {
	return accumKeyPrefix + namespace + ":" + agentName + ":" + model
}

// parseAccumKeySuffix splits "{agent}:{model}" into its parts.
// Returns ok=false if the suffix has no separator.
func parseAccumKeySuffix(suffix string) (agent, model string, ok bool) {
	idx := strings.Index(suffix, ":")
	if idx < 0 {
		return "", "", false
	}
	return suffix[:idx], suffix[idx+1:], true
}
