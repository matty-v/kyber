# metricsstore — Redis-backed metrics storage

`pkg/metricsstore` and `pkg/statechangestore` provide always-on storage for the five time-series Metrics tab panels, eliminating the Prometheus TSDB dependency for the common case.

## Key schema

All keys are scoped to the Kubernetes namespace so a single Redis instance can serve multiple Kyber clusters.

| Key | Type | Purpose |
|-----|------|---------|
| `ts:activity:{namespace}:{agent}:{state}` | Sorted set | Per-agent activity time-series; score = Unix timestamp, member = `"{ts}:{value}"` |
| `ts:token_usage:{namespace}:{agent}:{model}:{type}` | Sorted set | Per-(agent,model,type) token-delta time-series; `type` ∈ `input`/`cache_creation`/`cache_read`/`output`; score = Unix timestamp. Windowed source for the Token Usage panel (kyber#428) |
| `accum:state_changes:{namespace}:{agent}` | Hash | Accumulated state-transition counts; field = `to_state`, value = count |
| `accum:token_usage:{namespace}:{agent}:{model}` | Hash | Accumulated all-time token counts; fields = `input`, `cache_creation`, `cache_read`, `output` (a missing `output` field on a pre-upgrade hash reads as 0). Retained as the (agent,model) enumerator + all-time view alongside the windowed `ts:token_usage:*` series. After the `output` field's deploy, mixed query windows under-report output for up to 7 days (retention == max query window) and then self-heal |
| `node:{namespace}:{node}` | Hash | Latest node resource sample; fields = `cpuPercent`, `memUsedBytes`, etc. |

## Interfaces

```
pkg/metricsstore
  MetricsStore     AddPoint(key, ts, value) / RangeQuery(key, start, end)
  NodeStore        PutNode(namespace, node, sample) / GetAllNodes(namespace)
  Evictable        EvictBefore(olderThanTs)        — implemented by Redis stores

pkg/statechangestore
  Accumulator      IncrBy(namespace, agent, toState, delta) / GetAll(namespace)
```

## Implementations

| Implementation | Backed by | Use case |
|---|---|---|
| `RedisMetricsStore` | Redis sorted sets + ZADD/ZRANGEBYSCORE | Production |
| `MemoryMetricsStore` | In-process slice, sorted | Dev/test, no Redis |
| `RedisNodeStore` | Redis hash + HSET/HGETALL | Production |
| `MemoryNodeStore` | In-process map | Dev/test, no Redis |
| `RedisAccumulator` (statechangestore) | Redis hash + HINCRBY | Production |
| `MemoryAccumulator` (statechangestore) | In-process map | Dev/test |

Selection happens at control-plane startup in `cmd/control-plane/main.go`:
- If `KYBER_METRICS_REDIS_STORE_ENABLED=true` (default) and Redis is reachable → Redis implementations
- Otherwise → memory implementations with a `WARN` log; data is lost on pod restart

## Delta-per-interval storage for activity series

The status-sidecar sends **cumulative totals** (running sum since pod start) in each snapshot. `InternalServer.handleStatusSnapshot` subtracts the prior snapshot value to produce a **per-interval delta** before calling `AddPoint`. This means:

- Stored values are deltas (e.g., `12.5` seconds in working state during the last 15s interval)
- The PWA sums all points in a query window: `points.reduce((sum, p) => sum + p.v, 0)` → total seconds in window
- This is equivalent to Prometheus `increase(counter[window])` on the Tier 2 path

The prior-snapshot map lives on `InternalServer` (in-process, per-agent). Pod restarts reset it; the first snapshot after a restart contributes its full cumulative value as the first delta.

## Retention

A background goroutine (`StartRetentionWorker`) runs every 5 minutes and calls `EvictBefore(now - retentionSeconds)`:
- Redis: `SCAN ts:*` + `ZREMRANGEBYSCORE … -inf <cutoff>`
- Memory: prune slice in-place

Default retention: 7 days (`KYBER_METRICS_RETENTION_SECONDS=604800`). The `SCAN ts:*` pattern matches **every** time-series key, so the `ts:token_usage:*` token series (kyber#428) is evicted automatically with no retention-code change. Only time-series keys are evicted; accumulator hashes and node hashes are not touched (they hold all-time counts / latest samples, not a time series).

## Concurrency

- All implementations are goroutine-safe.
- `MemoryMetricsStore` uses a `sync.RWMutex` on the per-key slice.
- `MemoryNodeStore` uses a `sync.RWMutex` on the node map.
- `handleStatusSnapshot` holds a per-server `sync.Mutex` only while computing and updating the prior-snapshot map (fast path); `AddPoint` calls happen outside the lock.
- Redis operations are atomic at the command level; HINCRBY is atomic by design.

## Trade-offs vs Prometheus TSDB

| Concern | Redis store | Prometheus TSDB |
|---|---|---|
| Availability | Always-on (same Redis used by rest of control plane) | Requires separate OTLP → collector → Prometheus stack |
| Historical depth | Configurable (default 7 days) | Unlimited (depends on Prometheus retention) |
| Downsampling | None — raw 15s points returned | Recording rules can pre-aggregate |
| Alerting | Not supported | Full alerting rule support |
| External dashboards | Not directly accessible | Grafana, etc. can query Prometheus |
| Data on pod restart | Persisted in Redis | Persisted in TSDB |
| Data if Redis fails | Falls back to empty series | Unaffected |

## Cross-references

- `kyber#343` — implementation issue; acceptance criteria and design proposal
- `docs/architecture/metrics-data-flow.md` — end-to-end write and read path diagram
- `docs/operator/telemetry.md` — operator guide for enabling Redis store and optional Prometheus fallback
