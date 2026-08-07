package messagebuffer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// wakeKeyPrefix is the Redis key prefix for buffered wake messages.
	wakeKeyPrefix = "wake:"

	// defaultTTL is the time-to-live for buffered messages in Redis.
	// If the agent does not wake within this window, messages are lost.
	// Spec says 5 minutes; the controller emits a telemetry alert if wake takes longer.
	defaultTTL = 5 * time.Minute
)

// RedisBuffer is a Redis-backed MessageBuffer. Messages are stored as a Redis list
// under the key "wake:{agent-name}" with a 5-minute TTL. Drain is atomic: it reads
// and deletes the list in a single transaction.
type RedisBuffer struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedisBuffer returns a RedisBuffer backed by the given Redis client.
// TTL defaults to 5 minutes (as per spec).
func NewRedisBuffer(client *redis.Client) *RedisBuffer {
	return &RedisBuffer{client: client, ttl: defaultTTL}
}

// Push JSON-encodes msg and appends it to the Redis list for agentName.
// The list TTL is refreshed to 5 minutes on every Push.
func (r *RedisBuffer) Push(ctx context.Context, agentName string, msg PendingMessage) error {
	key := wakeKeyPrefix + agentName
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}
	pipe := r.client.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.Expire(ctx, key, r.ttl)
	if _, err = pipe.Exec(ctx); err != nil {
		return fmt.Errorf("pushing message to redis: %w", err)
	}
	return nil
}

// Drain atomically reads and deletes all buffered messages for agentName.
// Returns an empty (non-nil) slice if no messages are buffered.
func (r *RedisBuffer) Drain(ctx context.Context, agentName string) ([]PendingMessage, error) {
	key := wakeKeyPrefix + agentName

	var raws []string
	var lrangeCmd *redis.StringSliceCmd

	_, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		lrangeCmd = pipe.LRange(ctx, key, 0, -1)
		pipe.Del(ctx, key)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("draining wake buffer from redis: %w", err)
	}

	raws, err = lrangeCmd.Result()
	if err != nil {
		return nil, fmt.Errorf("reading lrange result: %w", err)
	}

	if len(raws) == 0 {
		return []PendingMessage{}, nil
	}

	msgs := make([]PendingMessage, 0, len(raws))
	for _, raw := range raws {
		var msg PendingMessage
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			return nil, fmt.Errorf("unmarshaling message: %w", err)
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// Delete discards the wake buffer for agentName. DEL of a missing key is a
// no-op, so this is idempotent (kyber#565).
func (r *RedisBuffer) Delete(ctx context.Context, agentName string) error {
	if err := r.client.Del(ctx, wakeKeyPrefix+agentName).Err(); err != nil {
		return fmt.Errorf("deleting wake buffer from redis: %w", err)
	}
	return nil
}
