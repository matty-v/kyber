package statechangestore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

const accumKeyPrefix = "accum:state_changes:"

// RedisAccumulator is a Redis-backed Accumulator. Each (namespace, agent) pair
// is stored as a hash at key accum:state_changes:{namespace}:{agent} with one
// field per to_state. Counts are incremented with HINCRBY for concurrent safety.
// No TTL is applied — counts are permanent.
type RedisAccumulator struct {
	client *redis.Client
}

// NewRedisAccumulator returns a RedisAccumulator backed by client.
func NewRedisAccumulator(client *redis.Client) *RedisAccumulator {
	return &RedisAccumulator{client: client}
}

// IncrBy atomically increments the transition count for (namespace, agent, toState).
func (a *RedisAccumulator) IncrBy(ctx context.Context, namespace, agentName, toState string, delta int64) error {
	if delta <= 0 {
		return nil
	}
	key := accumKeyPrefix + namespace + ":" + agentName
	return a.client.HIncrBy(ctx, key, toState, delta).Err()
}

// DeleteAgent removes the state-change hash for (namespace, agentName). DEL of a
// missing key is a no-op, so this is idempotent (kyber#565).
func (a *RedisAccumulator) DeleteAgent(ctx context.Context, namespace, agentName string) error {
	key := accumKeyPrefix + namespace + ":" + agentName
	if err := a.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("deleting state-change key %q: %w", key, err)
	}
	return nil
}

// GetAll scans for all accum:state_changes:{namespace}:* keys and returns counts.
func (a *RedisAccumulator) GetAll(ctx context.Context, namespace string) ([]AgentStateCounts, error) {
	pattern := accumKeyPrefix + namespace + ":*"
	nsPrefix := accumKeyPrefix + namespace + ":"

	var keys []string
	iter := a.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scanning state-change keys: %w", err)
	}

	var out []AgentStateCounts
	for _, key := range keys {
		agentName := strings.TrimPrefix(key, nsPrefix)
		fields, err := a.client.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("reading state-change key %q: %w", key, err)
		}
		for toState, countStr := range fields {
			count, _ := strconv.ParseInt(countStr, 10, 64)
			out = append(out, AgentStateCounts{
				Agent:   agentName,
				ToState: toState,
				Count:   count,
			})
		}
	}
	return out, nil
}
