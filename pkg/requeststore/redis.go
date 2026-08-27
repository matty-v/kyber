package requeststore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const requestKeyPrefix = "agentrequest:"

var createScript = redis.NewScript(`
local nowParts = redis.call('TIME')
local now = (tonumber(nowParts[1]) * 1000) + math.floor(tonumber(nowParts[2]) / 1000)
local expired = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now)
for _, id in ipairs(expired) do
  redis.call('DEL', ARGV[1] .. id)
end
if #expired > 0 then redis.call('ZREM', KEYS[2], unpack(expired)) end
if redis.call('EXISTS', KEYS[1]) == 1 then return {1} end
if redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[2]) then return {2} end
local expires = now + tonumber(ARGV[3])
redis.call('HSET', KEYS[1],
  'id', ARGV[4], 'agent', ARGV[5], 'prompt', ARGV[6], 'correlation', ARGV[7],
  'status', 'queued', 'response', '', 'failure_code', '',
  'created_at', now, 'expires_at', expires, 'updated_at', now)
redis.call('PEXPIREAT', KEYS[1], expires)
redis.call('ZADD', KEYS[2], expires, ARGV[4])
return {0, now, expires}
`)

var dispatchScript = redis.NewScript(`
local status = redis.call('HGET', KEYS[1], 'status')
if not status then return 1 end
if status == 'dispatched' then return 0 end
if status ~= 'queued' then return 2 end
local nowParts = redis.call('TIME')
local now = (tonumber(nowParts[1]) * 1000) + math.floor(tonumber(nowParts[2]) / 1000)
redis.call('HSET', KEYS[1], 'status', 'dispatched', 'updated_at', now)
return 0
`)

var failScript = redis.NewScript(`
local status = redis.call('HGET', KEYS[1], 'status')
if not status then return 1 end
if status == 'failed' and redis.call('HGET', KEYS[1], 'failure_code') == ARGV[1] then return 0 end
if status ~= 'queued' and status ~= 'dispatched' then return 2 end
local nowParts = redis.call('TIME')
local now = (tonumber(nowParts[1]) * 1000) + math.floor(tonumber(nowParts[2]) / 1000)
redis.call('HSET', KEYS[1], 'status', 'failed', 'failure_code', ARGV[1], 'updated_at', now)
redis.call('ZREM', KEYS[2], ARGV[2])
redis.call('ZADD', KEYS[3], now, ARGV[2])
while redis.call('ZCARD', KEYS[3]) > tonumber(ARGV[3]) do
  local removed = redis.call('ZPOPMIN', KEYS[3], 1)
  if #removed > 0 then redis.call('DEL', ARGV[4] .. removed[1]) end
end
return 0
`)

var completeScript = redis.NewScript(`
local status = redis.call('HGET', KEYS[1], 'status')
if not status then return 1 end
if status == 'completed' then
  if redis.call('HGET', KEYS[1], 'response') == ARGV[1] then return 0 end
  return 2
end
if status ~= 'dispatched' then return 2 end
local nowParts = redis.call('TIME')
local now = (tonumber(nowParts[1]) * 1000) + math.floor(tonumber(nowParts[2]) / 1000)
redis.call('HSET', KEYS[1], 'status', 'completed', 'response', ARGV[1], 'updated_at', now)
redis.call('ZREM', KEYS[2], ARGV[2])
redis.call('ZADD', KEYS[3], now, ARGV[2])
while redis.call('ZCARD', KEYS[3]) > tonumber(ARGV[3]) do
  local removed = redis.call('ZPOPMIN', KEYS[3], 1)
  if #removed > 0 then redis.call('DEL', ARGV[4] .. removed[1]) end
end
return 0
`)

// RedisStore is the durable, multi-replica request store. Lua scripts keep
// limits and lifecycle transitions atomic across control-plane replicas.
type RedisStore struct {
	client *redis.Client
	limits Limits
}

// NewRedisStore returns a Redis-backed store using limits.
func NewRedisStore(client *redis.Client, limits Limits) (*RedisStore, error) {
	if client == nil {
		return nil, ErrInvalidRequest
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &RedisStore{client: client, limits: limits}, nil
}

// Create atomically enforces the outstanding limit and writes a queued request.
func (r *RedisStore) Create(ctx context.Context, agent, id, prompt, correlation string) (*Request, error) {
	if err := validateCreate(agent, id, prompt, r.limits); err != nil {
		return nil, err
	}
	requestKey, outstandingKey, _, prefix := redisKeys(agent, id)
	result, err := createScript.Run(ctx, r.client, []string{requestKey, outstandingKey},
		prefix, r.limits.MaxOutstanding, r.limits.Lifetime.Milliseconds(), id, agent, prompt, correlation).Slice()
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	code, err := resultCode(result)
	if err != nil {
		return nil, err
	}
	switch code {
	case 1:
		return nil, ErrConflict
	case 2:
		return nil, ErrOutstandingLimit
	case 0:
	default:
		return nil, fmt.Errorf("creating request: unexpected script code %d", code)
	}
	if len(result) != 3 {
		return nil, errors.New("creating request: malformed script result")
	}
	createdMillis, err := integer(result[1])
	if err != nil {
		return nil, fmt.Errorf("decoding created time: %w", err)
	}
	expiresMillis, err := integer(result[2])
	if err != nil {
		return nil, fmt.Errorf("decoding expiry time: %w", err)
	}
	created := time.UnixMilli(createdMillis).UTC()
	return &Request{
		ID: id, Agent: agent, Prompt: prompt, Correlation: correlation, Status: StatusQueued,
		CreatedAt: created, ExpiresAt: time.UnixMilli(expiresMillis).UTC(), UpdatedAt: created,
	}, nil
}

// Get retrieves one live request without exposing any other agent's keyspace.
func (r *RedisStore) Get(ctx context.Context, agent, id string) (*Request, error) {
	requestKey, _, _, _ := redisKeys(agent, id)
	values, err := r.client.HGetAll(ctx, requestKey).Result()
	if err != nil {
		return nil, fmt.Errorf("getting request: %w", err)
	}
	if len(values) == 0 {
		return nil, ErrNotFound
	}
	return decodeRequest(values)
}

// MarkDispatched atomically moves a queued request to dispatched.
func (r *RedisStore) MarkDispatched(ctx context.Context, agent, id string) error {
	requestKey, _, _, _ := redisKeys(agent, id)
	value, err := dispatchScript.Run(ctx, r.client, []string{requestKey}).Result()
	return transitionResult("marking request dispatched", value, err)
}

// Fail atomically records a stable terminal failure.
func (r *RedisStore) Fail(ctx context.Context, agent, id string, code FailureCode) error {
	if code == "" {
		return ErrInvalidRequest
	}
	requestKey, outstandingKey, terminalKey, prefix := redisKeys(agent, id)
	value, err := failScript.Run(ctx, r.client, []string{requestKey, outstandingKey, terminalKey},
		string(code), id, r.limits.MaxTerminal, prefix).Result()
	return transitionResult("failing request", value, err)
}

// Complete atomically records the first response to a dispatched request.
func (r *RedisStore) Complete(ctx context.Context, agent, id, response string) error {
	if len([]byte(response)) > r.limits.MaxResponseBytes {
		return ErrResponseTooLarge
	}
	requestKey, outstandingKey, terminalKey, prefix := redisKeys(agent, id)
	value, err := completeScript.Run(ctx, r.client, []string{requestKey, outstandingKey, terminalKey},
		response, id, r.limits.MaxTerminal, prefix).Result()
	return transitionResult("completing request", value, err)
}

func redisKeys(agent, id string) (request, outstanding, terminal, requestPrefix string) {
	base := requestKeyPrefix + "{" + agent + "}:"
	requestPrefix = base + "request:"
	return requestPrefix + id, base + "outstanding", base + "terminal", requestPrefix
}

func transitionResult(operation string, value any, runErr error) error {
	if runErr != nil {
		return fmt.Errorf("%s: %w", operation, runErr)
	}
	code, err := integer(value)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	switch code {
	case 0:
		return nil
	case 1:
		return ErrNotFound
	case 2:
		return ErrConflict
	default:
		return fmt.Errorf("%s: unexpected script code %d", operation, code)
	}
}

func resultCode(values []any) (int64, error) {
	if len(values) == 0 {
		return 0, errors.New("requeststore: empty script result")
	}
	return integer(values[0])
}

func integer(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("requeststore: expected integer, got %T", value)
	}
}

func decodeRequest(values map[string]string) (*Request, error) {
	createdMillis, err := strconv.ParseInt(values["created_at"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("decoding created time: %w", err)
	}
	expiresMillis, err := strconv.ParseInt(values["expires_at"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("decoding expiry time: %w", err)
	}
	updatedMillis, err := strconv.ParseInt(values["updated_at"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("decoding updated time: %w", err)
	}
	return &Request{
		ID: values["id"], Agent: values["agent"], Prompt: values["prompt"], Correlation: values["correlation"],
		Status: Status(values["status"]), Response: values["response"], FailureCode: FailureCode(values["failure_code"]),
		CreatedAt: time.UnixMilli(createdMillis).UTC(), ExpiresAt: time.UnixMilli(expiresMillis).UTC(),
		UpdatedAt: time.UnixMilli(updatedMillis).UTC(),
	}, nil
}

var _ Store = (*RedisStore)(nil)
