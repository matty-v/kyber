# Kyber Helm Chart

Deploys the Kyber control plane (controller manager + public API server) and its supporting resources into a Kubernetes cluster.

## Dependencies

### Redis (optional, recommended for production)

Redis is used for:

- **Token Usage accumulator (Tier 1)** — persistent per-agent/model token counts for the Metrics tab Token Usage panel. When Redis is configured, the panel is always available without a Prometheus dependency. Without Redis, the panel falls back to Prometheus TSDB (Tier 2) or shows empty state.
- **Inbound deduplication** — guards against duplicate webhook POSTs (via `RedisDeduper`).
- **Inbound envelope cache** — stores rendered envelopes for replay (7-day TTL).
- **Token snapshot store** — caches the most recent Claude Code context-budget snapshot per agent (5-minute TTL).

Set `redis.url` in your values override to enable Redis-backed features:

```yaml
redis:
  url: "redis://redis.kyber-system.svc.cluster.local:6379"
```

Without `redis.url`, all Redis-backed features degrade gracefully: the token accumulator is absent (Tier 2 TSDB fallback), dedup is skipped (fail-open), and the envelope cache is disabled.

### Prometheus (optional)

Required for time-series panels in the Metrics tab (Agent Activity, Working Time Trend, Node Resources, State Change Frequency). Not required for CRD-backed panels (Fleet Summary, Last Active) or the Token Usage panel when Redis is configured.

Set `metrics.prometheusURL` in your values override:

```yaml
metrics:
  prometheusURL: "http://prometheus.monitoring.svc.cluster.local:9090"
```

## Operations

### Platform logging

All Kyber-managed pods carry `app.kubernetes.io/part-of=kyber` for fleet log
discovery and external collectors. Configure global/component verbosity and
durable retention under `logging`; configure the optional Vector writer under
`logShipper`. See [Operating Kyber logs](../../../docs/operator/logging.md) for
collector recipes, retention responsibilities, failure behavior, and API/PWA
usage.

### Status sidecar image convergence

Bumping `image.statusSidecar.tag` and reapplying the chart (e.g. ArgoCD sync after a release) automatically converges every existing agent pod to the new sidecar image — no manual `kubectl delete pod` needed. Mechanism: the Agent controller diffs each pod's `kyber-status-sidecar` container spec image against its current `KYBER_STATUS_SIDECAR_IMAGE` env on every reconcile; on mismatch it deletes the pod, and the next reconcile rebuilds it on the new image. Convergence completes within one reconcile cycle per agent (≤2 minutes for typical fleets). See kyber#358.

This convergence is unconditional and tag-level. A Working agent may be interrupted by the rebuild on a sidecar bump — explicit design trade-off in favor of "metrics flowing end-to-end" over "never interrupt a Working agent for a sidecar skew." Operators on digest pins retain the gentler kyber#299 Option B auto-roll (idle gate, stability window, concurrency cap) via `controlPlane.sidecarAutoRollEnabled`.

### Metrics: OTel endpoint (opt-in)

The kyber-status-sidecar's OTLP push is **disabled by default** (`metrics.otelEndpoint: ""`). The kyber#343 heartbeat path — sidecar POSTs metrics-snapshot data to `/internal/agents/{name}/status` every 15s, written to `pkg/metricsstore` (Redis sorted sets) — already carries every in-tree Metrics tab consumer (Agent Activity, Working Time, State Changes, Node Resources, Token Usage). No collector deployment is required.

Set `metrics.otelEndpoint` only if you run an external OTel pipeline that consumes per-agent metrics:

```yaml
metrics:
  otelEndpoint: "http://otel-collector.monitoring:4318"
```

When empty, the control plane omits `KYBER_OTEL_ENDPOINT` from the injected sidecar env entirely — no DNS lookups, no failed uploads, no log noise. The legacy `controlPlane.otelEndpoint` value is preserved for backwards compatibility (coalesced in if set) but deprecated; prefer `metrics.otelEndpoint`.

## Values reference

See `values.yaml` for all configurable parameters with inline documentation.
