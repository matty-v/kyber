# Metrics Tab Data Flow

The Metrics tab combines three sources behind the control-plane API:

| Panels | Primary source | Optional fallback |
|---|---|---|
| Fleet Summary, Last Active | Agent CRDs | None |
| Activity, Working Time, Node Resources, State Changes | Redis time series and state-change counters | Prometheus TSDB |
| Token Usage | PostgreSQL per-call usage evidence | None |

The PWA calls the control plane. It never reads Redis, Prometheus, or PostgreSQL directly.

## Activity and resource metrics

The status sidecar reports cumulative activity and state changes every 15 seconds. The control plane stores activity deltas in `ts:activity:{namespace}:{agent}:{state}` and transition counts in `accum:state_changes:{namespace}:{agent}`. The node agent reports resource samples every 30 seconds. Redis retention defaults to seven days. When enabled, OTLP export supplies the Prometheus fallback.

## Token Usage

Each harness reporter reads local records of completed model calls. Claude uses finalized transcript records, Codex derives deltas from cumulative rollout counters, and Hermes reads API-call summary log lines. Reporters persist source cursors on the Agent volume and send evidence through the status sidecar. The control plane resolves the current Agent UID and configured route, then stores accepted events and gap records in PostgreSQL. Corrected events update daily aggregates in the same transaction.

`GET /api/v1/usage/events` provides paginated per-call evidence for an exact time range. `GET /api/v1/usage/aggregates?bucket=day` provides daily summaries. Both require `usage:read` and filter to the caller's current Agent UIDs. Pagination is pinned to a short-lived read snapshot. The API preserves unknown token classes and upstream providers as unknown and reports detected gaps. It does not calculate dollar costs.

The Metrics tab groups the selected events by Agent, model, configured route, and observed upstream provider. If the chosen range exceeds the browser's bounded event fetch, it asks for a shorter range rather than showing a partial total. Detailed events default to 90 days of retention; daily aggregates default to 13 months.

The live `GET /api/v1/agents/{name}/token-usage` endpoint is separate. It reads a short-lived reporter snapshot from Redis for the context-window display. Those snapshots do not generate accounting totals.

## Access and operations

All API reads pass through the cluster's existing authentication and scope checks. A deleted Agent's UID is removed with its evidence during finalizer cleanup, so a new Agent with the same name does not inherit its history. Redis or Prometheus outages affect their respective panels; PostgreSQL availability determines Token Usage availability.

See [the operator telemetry guide](../operator/telemetry.md) and [the local usage metadata design](../design/2026-10-11-local-usage-metadata.md).
