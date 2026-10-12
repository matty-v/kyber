# Telemetry and the Metrics Tab

The Metrics tab reads different stores for different panels:

1. Fleet Summary and Last Active read Agent CRDs.
2. Activity, Working Time, Node Resources, and State Changes use Redis time series by default, with optional Prometheus history.
3. Token Usage reads durable per-call evidence from PostgreSQL through `GET /api/v1/usage/events`. It reports observed token classes, model, configured route, and upstream provider when known. Unknown counts stay unknown; Kyber does not calculate dollar costs.

## Default (no-Prometheus) mode

With `metrics.redisStoreEnabled: true`, the activity, working-time, node, and state-change panels populate without Prometheus. Their Redis history defaults to 7 days (`metrics.redisRetentionSeconds: 604800`). Token Usage needs PostgreSQL and an Agent reporter that emits local usage evidence; it does not depend on Redis or Prometheus. Detailed usage events default to 90 days of retention and daily aggregates to 13 months.

To disable the Redis time-series store and use Prometheus for its panels, set `metrics.redisStoreEnabled: false`. This does not change Token Usage.

## Enabling OTLP Export

By default, `telemetry.enabled` is `false`. Redis-backed panels still work; enable OTLP export when you want Prometheus history or external dashboards.

### Step 1: Deploy an OTEL Collector + Prometheus backend

The Kyber chart does not include a Prometheus deployment. You need a Prometheus-compatible TSDB with a remote-write endpoint that accepts OTLP. Options:

- **Prometheus + OTEL Collector**: Deploy Prometheus with `--enable-feature=otlp-write-receiver` and expose its OTLP HTTP port (default `4318`).
- **VictoriaMetrics**: Exposes a native OTLP ingestion endpoint.
- **Grafana Mimir / Thanos**: Also compatible.

### Step 2: Configure Helm values

```yaml
telemetry:
  enabled: true
  otlpEndpoint: "http://otel-collector.monitoring:4318"  # OTLP HTTP endpoint

metrics:
  prometheusURL: "http://prometheus.monitoring:9090"     # Prometheus query API
  stalenessThresholdSeconds: 300                         # 5 min (default)
```

### Step 3: Verify data flow

After deploying, verify metrics are flowing:

```bash
# From inside the cluster
curl http://prometheus.monitoring:9090/api/v1/query \
  '?query=kyber_agent_activity_state_seconds_total' | jq .data.result
```

If the result is empty, check:

1. **OTLP endpoint reachable** — the control-plane pod and node-agent pods must be able to reach `telemetry.otlpEndpoint`.
2. **Collector forwarding to Prometheus** — verify the collector's `prometheus_remote_write` exporter is configured and healthy.
3. **Label cardinality** — the `namespace` label on all Kyber metrics matches the namespace your cluster runs in (`kyber-system` by default).

## Metrics Produced

| Metric | Labels | Source | Panel |
|--------|--------|--------|-------|
| `kyber_agent_activity_state_seconds_total` | agent, state, namespace | status-sidecar | Activity, Working Time |
| `kyber_agent_heartbeat_seconds_since_last` | agent, namespace | status-sidecar | (Last Active uses CRD) |
| `kyber_agent_state_changes_total` | agent, to_state, namespace | control-plane | State Changes |
| `node_cpu_seconds_total` | mode, node, namespace | node-agent | Node Resources |
| `node_memory_MemTotal_bytes` | node, namespace | node-agent | Node Resources |
| `node_memory_MemAvailable_bytes` | node, namespace | node-agent | Node Resources |
| `node_filesystem_size_bytes` | node, mountpoint, namespace | node-agent | Node Resources |
| `node_filesystem_free_bytes` | node, mountpoint, namespace | node-agent | Node Resources |

## Usage metadata API

`GET /api/v1/usage/events` returns authorized, paginated per-call evidence. `GET /api/v1/usage/aggregates?bucket=day` returns daily summaries. Both require `usage:read` and are restricted to the caller's current Agent UIDs. Responses include nullable `inputUncached`, `output`, `cacheRead`, and `cacheWrite` counts, a detected-gap count, and no cost field. A missing upstream provider is shown as unknown; a configured OpenRouter route does not prove which provider served a request.

The Metrics tab groups observed calls for the selected time range. If a range contains too many events to summarize without truncation, choose a shorter range. Cost calculations can be performed by a separate system using this API and its own pricing data.

## TLS for Prometheus

By default, TLS verification is enforced on `metrics.prometheusURL`. For clusters with internal self-signed certificates:

```yaml
metrics:
  prometheusInsecureSkipVerify: true
```

Only use this for cluster-internal URLs that are not exposed to the internet.

## Performance Notes

- The 30-second Node Resources poll generates at most one Prometheus instant query per 20 seconds per cluster (server-side cache), regardless of concurrent PWA sessions.
- Fleet Summary and Last Active responses are cached for 15 seconds on the control plane.
- Prometheus range queries over 7 days at high agent cardinality may be slow. If p99 latency exceeds 5 seconds, consider adding recording rules for the most expensive aggregations (follow-up issue planned).

## Platform alert delivery

The control plane raises **platform alerts** for notable events (e.g. the Phase-C
`SidecarOOMRestart`). Alerts always go to the controller log (the **`LogAlertSink`
floor**) and, when a webhook is configured, are also **POSTed to a receiver** so they
can become phone-actionable (Telegram, an Echo Base route, etc.).

### Configuration and the startup warning

Delivery is enabled by setting **`KYBER_ALERT_WEBHOOK_URL`** on the control-plane
Deployment (the Helm chart wires it, defaults-empty). The URL may embed an operator
secret/token (see Auth below).

- **Configured** (`KYBER_ALERT_WEBHOOK_URL` set): alerts are logged **and** delivered.
- **Unconfigured** (empty): the control plane logs a **loud startup warning** that
  platform alerts are **log-only and not delivered anywhere**, and continues running.
  This is **fail-loud, not fail-closed** — a missing webhook never blocks boot or
  degrades the control plane; it just makes the "no delivery" state impossible to miss:

  ```
  WARN: KYBER_ALERT_WEBHOOK_URL is unset — platform alerts (incl. Phase-C
  SidecarOOMRestart) are LOG-ONLY and are NOT delivered to any phone-actionable
  receiver. Set KYBER_ALERT_WEBHOOK_URL to enable delivery.
  ```

> Provisioning the receiver and the `KYBER_ALERT_WEBHOOK_URL` value is a maintainer
> task and is intentionally out of the control-plane image.

### Receiver contract

A receiver (Echo Base route, Telegram bridge, or anything else) can be built against
this stable, receiver-agnostic contract.

**Transport**
- HTTP `POST` to `KYBER_ALERT_WEBHOOK_URL`, `Content-Type: application/json`.
- Client timeout: **5 seconds**.
- **Best-effort, fire-once**: the sender de-duplicates per escalation (it does not
  re-POST the same alert). Delivery failures and non-2xx responses are **logged
  non-fatally and NOT retried** — a slow or broken receiver must never stall the
  control-plane reconcile loop. Treat delivery as at-most-once.

**Payload** — a stable envelope; alert-type-specific fields live under `details`:

```json
{
  "timestamp": "2026-06-15T02:50:00Z",
  "severity":  "warning",
  "kind":      "agent",
  "name":      "agent-r2-d2",
  "reason":    "SidecarOOMRestart",
  "details": {
    "sidecar":      "transcript-tailer",
    "restartCount": "4",
    "condition":    "flapping",
    "threshold":    "3"
  }
}
```

| Field | Type | Meaning |
|---|---|---|
| `timestamp` | string | RFC3339, UTC — when the alert fired. |
| `severity` | string | `info` \| `warning` \| `critical`. |
| `kind` | string | Resource type, e.g. `agent`, `machine`. |
| `name` | string | Resource name (e.g. the agent name). |
| `reason` | string | Machine-readable alert type, e.g. `SidecarOOMRestart`. |
| `details` | object (string→string) | Alert-type-specific context. For `SidecarOOMRestart`: `sidecar`, `restartCount`, `condition` (`flapping` \| `OOMKilled`), `threshold`. Omitted when empty. |

The envelope (`timestamp`/`severity`/`kind`/`name`/`reason`/`details`) is stable as new
alert types are added — new types vary only the `reason` and the `details` keys.

**Auth / trust model**
- The trust model is the **operator-provisioned secret embedded in the URL**: any
  caller able to reach the secret URL is trusted, and the receiver verifies by URL
  secrecy. The sender does **not** sign the request (no HMAC header) in this version.
- Delivery is **outbound-only** — it adds no inbound surface to the control plane.
- **Redaction guarantee:** the sender **never logs the webhook URL** (which can embed
  the secret), including on delivery failure — only the host[:port] is logged for
  diagnosis. (A future receiver MAY additionally require a signature header; that is an
  optional, additive hardening, not part of this contract.)
