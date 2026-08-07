# Telemetry and the Metrics Tab

The Kyber Metrics tab has three data tiers:

1. **CRD-backed panels** (Fleet Summary, Last Active) — always available; read agent CRD status fields directly.
2. **Redis-backed panels** (Activity, Working Time, Tokens, Node Resources, State Changes) — populated automatically from the status-sidecar and node-agent; no Prometheus required. On by default (`metrics.redisStoreEnabled: true`).
3. **Prometheus TSDB** — optional fallback and historical source for the same five panels. Used when Redis is disabled or when historical data predates the Redis store rollout.

## Default (no-Prometheus) mode

With `metrics.redisStoreEnabled: true` (the default), all five time-series panels populate without any Prometheus setup. The status-sidecar and node-agent write directly to Redis every 15–30 seconds.

**What works out of the box:**
- All 7 Metrics tab panels populate once pods are running
- Up to 7 days of history (`metrics.redisRetentionSeconds: 604800`)
- Token usage and state-change counts are always-on (Redis accumulators)

**What you lose without Prometheus:**
- Historical data predating the Redis store (before cluster upgrade to this version)
- Alerting rules and recording rules
- External dashboards (Grafana, etc.) that query Prometheus directly

To disable the Redis store and use only Prometheus: set `metrics.redisStoreEnabled: false` in your Helm values.

## Enabling OTLP Export

By default, `telemetry.enabled` is `false`. Time-series panels show an empty state until telemetry is enabled and metrics are flowing.

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
  tokenRatesConfigMapName: kyber-provider-rates          # default, created by chart
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
| `kyber_agent_tokens_total` | agent, model, token_type, namespace | control-plane | Token Usage |
| `kyber_agent_state_changes_total` | agent, to_state, namespace | control-plane | State Changes |
| `node_cpu_seconds_total` | mode, node, namespace | node-agent | Node Resources |
| `node_memory_MemTotal_bytes` | node, namespace | node-agent | Node Resources |
| `node_memory_MemAvailable_bytes` | node, namespace | node-agent | Node Resources |
| `node_filesystem_size_bytes` | node, mountpoint, namespace | node-agent | Node Resources |
| `node_filesystem_free_bytes` | node, mountpoint, namespace | node-agent | Node Resources |

## Token Cost Rates

Provider rates are stored in the `kyber-provider-rates` ConfigMap (created by the Helm chart at `templates/configmap-rates.yaml`). To update rates:

```bash
kubectl edit configmap kyber-provider-rates -n kyber-system
```

Format (`provider-rates.yaml` key in the ConfigMap data):

```yaml
claude-sonnet-4-6:
  input: 3.0        # USD per 1M input tokens
  output: 15.0      # USD per 1M output tokens
  cache_read: 0.3   # USD per 1M cache-read tokens
```

The control plane reads this file at request time (no restart required after editing). Models not in the table show $0 cost.

Cost computation requires `metrics.tokenRatesConfigMapName` to be set (default: `kyber-provider-rates`). When it is empty, the chart omits the ConfigMap, the mount, and `KYBER_METRICS_TOKEN_RATES_PATH`, and all costs degrade gracefully to $0 (token counts are unaffected).

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
