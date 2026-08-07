# Metrics Tab Data Flow

## Default (Redis-backed) mode — kyber#343

All five time-series panels read from Redis first (Tier 1) and fall back to Prometheus only when Redis is empty or not configured.

```
┌─────────────────────────────────────────────────────────────────────┐
│ Kyber PWA (browser)                                                 │
│   MetricsTab ─ TimeRangePicker + 7 panel components                │
│   src/lib/api.ts  ──► GET /api/v1/metrics/*                        │
└────────────────────────────┬────────────────────────────────────────┘
                             │ cluster API key
                             ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Kyber control plane  pkg/api/routes_metrics.go                      │
│                                                                     │
│  Tier 1 (Redis) ◄─────────────────────────────────────┐            │
│    pkg/metricsstore  — activity + working-time series  │            │
│    pkg/statechangestore — state-change counts          │            │
│    pkg/tokenstore     — token accumulator              │            │
│                                                        │            │
│  Tier 2 (Prometheus) — fallback when Tier 1 is empty  │            │
│    pkg/metrics/tsdb.go — PromQL range/instant queries  │            │
│                                                                     │
│  Always-available (CRD-backed):                                     │
│    pkg/metrics/summary.go — fleet counts + last-active             │
└──────┬─────────────────────────────────┬──────────┬────────────────┘
       │ CRD list/watch                  │ Redis    │ PromQL HTTP
       ▼                                 ▼          ▼
┌──────────────┐    ┌───────────────────────┐  ┌───────────────────┐
│ Kubernetes   │    │ Redis                 │  │ Prometheus-compat │
│ Agent CRDs   │    │ ts:activity:{ns}:*    │  │ TSDB (optional)   │
│ (status,     │    │ ts:token_usage:{ns}:* │  │ Receives OTLP     │
│  lastHB)     │    │ accum:state_changes:* │  │ from OTel         │
└──────────────┘    │ accum:token_usage:*   │  │ Collector         │
                    │ node:{ns}:*           │  └───────────────────┘
                    └───────────────────────┘
```

### Write path (Tier 1)

```
status-sidecar (per agent pod)                 node-agent (per node)
   BumpActivityCounter()                          Collector.Collect()
   DrainSnapshot() → {stateSecs, transitions}     ↓
   POST /internal/agents/{name}/status            ResourceReporter.Report()
   (every 15s)                                    POST /internal/nodes/{name}/resources
        ↓                                         (every 30s)
   InternalServer.handleStatusSnapshot()       InternalServer.handleNodeResources()
   - compute delta vs prior snapshot              - validate DNS-1123 + ranges
   - AddPoint(ts:activity:{ns}:{agent}:{state})   - PutNode(node:{ns}:{node})
   - IncrBy(accum:state_changes:{ns}:{agent})
```

The sidecar sends **cumulative totals** (running sum since pod start). The handler subtracts the prior snapshot value to store **per-interval deltas**. The PWA sums all delta points in a query window to compute the total — equivalent to Prometheus `increase()` on the counter series.

Token usage is written via `POST /internal/agents/{name}/token-usage`. The handler computes the per-report delta and writes it to **both** Tier-1 sources: `tokenAccumulator.IncrBy(accum:token_usage:{ns}:{agent}:{model})` for the all-time totals, and `MetricsStore.AddPoint(ts:token_usage:{ns}:{agent}:{model}:{type})` for each non-zero type component (`input`/`cache_creation`/`cache_read`) so the windowed read can honor `start`/`end` (kyber#428). The windowed series stores deltas and is summed over the window, identical to the activity series.

### Tier preference per panel

| Panel | Tier 1 source | Tier 2 fallback |
|---|---|---|
| Activity Breakdown | `MetricsStore.RangeQuery(ts:activity:*)` | `increase(kyber_agent_activity_state_seconds_total[window])` |
| Working Time | `MetricsStore.RangeQuery(ts:activity:*:working)` | `increase(kyber_agent_activity_state_seconds_total{state="working"}[window])` |
| Token Usage | `MetricsStore.RangeQuery(ts:token_usage:*)` — windowed; `TokenAccumulator.GetAll()` enumerates the (agent,model) pairs and is retained as the all-time view | `increase(kyber_agent_tokens_total[window])` evaluated **at the window end** (absolute window, not anchored to now) |
| State Changes | `StateChangeAccumulator.GetAll()` | `increase(kyber_agent_state_changes_total[window])` |
| Node Resources | `NodeStore.GetAllNodes()` | Prometheus node_* metrics |
| Fleet Summary | Agent CRD (always) | — |
| Last Active | Agent CRD (always) | — |

## Token pricing (cost) — feed-derived, fail-loud

The Token Usage panel's cost column is computed server-side from a **per-model
rate table**, not stored. Pricing is **not** hand-maintained and the third party
that supplies it never enters the runtime path (kyber#487). LiteLLM here is a
**build-time data source only — not a runtime proxy/gateway**; no agent/LLM
traffic routes through it. For the end-to-end onboarding architecture (this
pricing path **and** the context-window auto-detect path) see
[`model-onboarding.md`](model-onboarding.md):

```
LiteLLM model_prices_and_context_window.json  (public, MIT, pinned commit)
        │  weekly refresh-bot PR (.github/workflows/refresh-provider-rates.yml)
        ▼
cmd/fetch-provider-rates  →  metrics.ProjectLiteLLM
        │  provider filter (anthropic, openai) + per-token→per-MTok + sanity bounds
        ▼
deploy/helm/kyber/files/provider-rates.generated.yaml  (vendored, reviewed in the bot PR)
   + provider-rates.generated.meta  (upstream commit + fetched_at stamp)
        │  chart renders via .Files.Get → kyber-provider-rates ConfigMap
        ▼
KYBER_METRICS_TOKEN_RATES_PATH → metrics.LoadRateTable (runtime read, unchanged)
        ▼
RateTable.ComputeCostKnown(model, counts) → (cost, priced)
```

- **Fail-loud `priced` sentinel.** `ComputeCostKnown` returns `(cost, priced)`.
  `priced=false` means the model has no rate; the API's `TokenUsageResponse.priced`
  carries it, and the PWA renders a `—`/`unpriced` badge (mirrors the
  `contextWindowKnown` idiom) instead of a believable `$0.0000`. A missing or
  rejected rate is always visible, never a fake zero.
- **Key normalization.** Lookup strips a trailing `-YYYYMMDD` release-date suffix
  from both the model id and the rate keys, so bare↔dated variants resolve
  (`claude-haiku-4-5` ↔ `claude-haiku-4-5-20251001`). Exact match wins.
- **Supply-chain posture (architecture A).** The feed is consumed at **build
  time** and every price change lands as a **reviewed PR diff** (Chewie reviews
  the refresh bot's PR) — a poisoned/erroneous upstream value is caught before
  it ships, not live in prod. The adapter applies **sanity bounds**
  (`0 < rate ≤ 10000`/MTok); an out-of-bounds value is rejected → `unpriced`
  rather than a confidently-wrong number. The runtime makes no external call.
- **CI gate — wired + fresh.** `scripts/preflight-model-pricing.sh`
  (in `test.yml`) asserts the vendored dataset exists, parses, is non-empty,
  every rate is within bounds, the chart renders it, and the freshness stamp is
  within 30 days — forcing the refresh bot to keep the snapshot current. There
  is **no** hand-maintained per-model allowlist; an uncovered model is simply
  `unpriced` and visible.
- **Code-owner review gate.** `.github/CODEOWNERS` makes the generated pricing
  files (`provider-rates.generated.{yaml,meta}`) require a code-owner (`@matty-v`)
  review. The refresh bot opens the PR but cannot self-merge the price diff —
  this is the architecture-A human gate that keeps the third-party feed out of
  the runtime path *and* stops a plausible-but-wrong price from shipping silently.

## Key design decisions

**Control plane proxy pattern** — the PWA never talks directly to Redis or Prometheus. All data access is server-side with validated inputs (DNS-1123 names, state allow-lists, range checks). No raw user input reaches Redis key construction.

**Delta-per-interval storage** — activity time-series points are deltas, not cumulative totals. This lets the PWA sum a window with `array.reduce((sum, p) => sum + p.v, 0)` — the same math works for both Tier 1 and Tier 2 data shapes.

**Auth** — all `/api/v1/metrics/*` endpoints use the existing cluster API key middleware. No new credentials are introduced.

**Empty-safe** — every handler returns a structured empty result (HTTP 200, `[]`) on timeout, unreachable TSDB, or no data. No panel propagates a 500.

**Memory fallback** — when Redis is unreachable at startup, all stores fall back to in-memory implementations with a WARN log. Data is lost on pod restart but the cluster stays operational.

## Cross-references

- `kyber#343` (closed) — Redis-backed store; `pkg/metricsstore`, `pkg/statechangestore`; `docs/architecture/metricsstore.md`
- `kyber#256` (closed) — OTel sidecar metrics; `kyber_agent_activity_state_seconds_total` is the Tier 2 source for Activity and Working Time panels.
- `docs/operator/telemetry.md` — operator guide for enabling OTLP export (Tier 2) and Redis store (Tier 1).
- `deploy/helm/kyber/templates/configmap-rates.yaml` — renders the vendored, feed-derived provider rates into the `kyber-provider-rates` ConfigMap used by the Token Usage panel (see § Token pricing above).
- `kyber#487` — fail-loud `unpriced` sentinel/badge + feed-derived pricing (this flow); `kyber#489` — model-onboarding epic.
