package metricsstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisMetricsStore is a Redis-backed MetricsStore. Time-series data is stored
// in sorted sets keyed as ts:activity:{namespace}:{agent}:{state} with score =
// unix timestamp (seconds) and member = "{ts}:{value}" (ts duplicated to
// disambiguate equal scores). No TTL is set on keys — eviction is handled by
// the retention worker (retention.go).
type RedisMetricsStore struct {
	client *redis.Client
}

// NewRedisMetricsStore returns a RedisMetricsStore backed by client.
func NewRedisMetricsStore(client *redis.Client) *RedisMetricsStore {
	return &RedisMetricsStore{client: client}
}

// AddPoint appends a (ts, value) point to the sorted set at key.
func (r *RedisMetricsStore) AddPoint(ctx context.Context, key string, ts int64, value float64) error {
	member := fmt.Sprintf("%d:%.6f", ts, value)
	return r.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(ts),
		Member: member,
	}).Err()
}

// RangeQuery returns points in [startTs, endTs] ordered by timestamp ascending.
func (r *RedisMetricsStore) RangeQuery(ctx context.Context, key string, startTs, endTs int64) ([]Point, error) {
	vals, err := r.client.ZRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min: strconv.FormatInt(startTs, 10),
		Max: strconv.FormatInt(endTs, 10),
	}).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("zrangebyscore %q: %w", key, err)
	}
	out := make([]Point, 0, len(vals))
	for _, z := range vals {
		member, ok := z.Member.(string)
		if !ok {
			continue
		}
		_, val, ok := parseMember(member)
		if !ok {
			continue
		}
		out = append(out, Point{Timestamp: int64(z.Score), Value: val})
	}
	return out, nil
}

// EvictBefore removes all time-series points with score < olderThanTs from every
// ts:* key in Redis. Called by the retention worker.
func (r *RedisMetricsStore) EvictBefore(ctx context.Context, olderThanTs int64) error {
	iter := r.client.Scan(ctx, 0, "ts:*", 0).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		if err := r.client.ZRemRangeByScore(ctx, key,
			"-inf",
			strconv.FormatInt(olderThanTs-1, 10),
		).Err(); err != nil {
			return fmt.Errorf("zremrangebyscore %q: %w", key, err)
		}
	}
	return iter.Err()
}

// DeleteAgent scans the per-agent ts:activity and ts:token_usage families for
// (namespace, agent) and deletes every matching key. DEL of a missing key is a
// no-op, so this is idempotent (kyber#565).
func (r *RedisMetricsStore) DeleteAgent(ctx context.Context, namespace, agent string) error {
	for _, prefix := range agentSeriesPrefixes(namespace, agent) {
		iter := r.client.Scan(ctx, 0, prefix+"*", 0).Iterator()
		var keys []string
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		if err := iter.Err(); err != nil {
			return fmt.Errorf("scanning %q for delete: %w", prefix, err)
		}
		if len(keys) == 0 {
			continue
		}
		if err := r.client.Del(ctx, keys...).Err(); err != nil {
			return fmt.Errorf("deleting metrics keys for %q: %w", prefix, err)
		}
	}
	return nil
}

// parseMember splits a "{ts}:{value}" member into its components.
func parseMember(m string) (ts int64, val float64, ok bool) {
	idx := strings.Index(m, ":")
	if idx < 0 {
		return 0, 0, false
	}
	t, err1 := strconv.ParseInt(m[:idx], 10, 64)
	v, err2 := strconv.ParseFloat(m[idx+1:], 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return t, v, true
}

// RedisNodeStore stores the latest resource sample per node in a Redis hash.
type RedisNodeStore struct {
	client *redis.Client
}

// NewRedisNodeStore returns a RedisNodeStore backed by client.
func NewRedisNodeStore(client *redis.Client) *RedisNodeStore {
	return &RedisNodeStore{client: client}
}

// PutNode writes the latest sample for namespace/node into the Redis hash.
func (r *RedisNodeStore) PutNode(ctx context.Context, namespace, node string, sample NodeSample) error {
	key := NodeKey(namespace, node)
	return r.client.HSet(ctx, key,
		"cpuPercent", strconv.FormatFloat(sample.CPUPercent, 'f', 6, 64),
		"memUsedBytes", strconv.FormatFloat(sample.MemUsedBytes, 'f', 0, 64),
		"memTotalBytes", strconv.FormatFloat(sample.MemTotalBytes, 'f', 0, 64),
		"diskUsedBytes", strconv.FormatFloat(sample.DiskUsedBytes, 'f', 0, 64),
		"diskTotalBytes", strconv.FormatFloat(sample.DiskTotalBytes, 'f', 0, 64),
		"updatedAt", sample.UpdatedAt,
	).Err()
}

// GetAllNodes scans for node:{namespace}:* keys and returns the latest sample for each.
func (r *RedisNodeStore) GetAllNodes(ctx context.Context, namespace string) ([]NodeSample, error) {
	pattern := "node:" + namespace + ":*"
	prefix := "node:" + namespace + ":"

	var keys []string
	iter := r.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scanning node keys: %w", err)
	}

	out := make([]NodeSample, 0, len(keys))
	for _, key := range keys {
		nodeName := strings.TrimPrefix(key, prefix)
		fields, err := r.client.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("reading node key %q: %w", key, err)
		}
		sample := NodeSample{Node: nodeName, Namespace: namespace}
		sample.CPUPercent, _ = strconv.ParseFloat(fields["cpuPercent"], 64)
		sample.MemUsedBytes, _ = strconv.ParseFloat(fields["memUsedBytes"], 64)
		sample.MemTotalBytes, _ = strconv.ParseFloat(fields["memTotalBytes"], 64)
		sample.DiskUsedBytes, _ = strconv.ParseFloat(fields["diskUsedBytes"], 64)
		sample.DiskTotalBytes, _ = strconv.ParseFloat(fields["diskTotalBytes"], 64)
		sample.UpdatedAt = fields["updatedAt"]
		out = append(out, sample)
	}
	return out, nil
}
