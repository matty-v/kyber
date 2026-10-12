# Metrics Tab

The Metrics tab (`/metrics`) is a read-only observability dashboard for the current cluster. It surfaces the most important signals — agent activity, observed token usage, node resource gauges, and reliability signals — without requiring an external observability stack.

## Navigation

The Metrics tab appears in the sidebar (desktop) and bottom nav bar (mobile) alongside Fleet, Machines, Agents, and Settings.

## Time Range Picker

The picker at the top right of the page controls the time window for all time-series panels:

| Option | Window |
|--------|--------|
| Last 15 min | 15 minutes |
| 1 h | 1 hour (default) |
| 6 h | 6 hours |
| 24 h | 24 hours |
| 7 d | 7 days |

Selecting a new range re-fetches all time-series panels simultaneously. In-flight requests from the previous range are cancelled. The **Node Resources** panel is unaffected by the time range — it always shows the current live state.

## Panels

### Fleet Summary

Four stat cards showing current-moment counts for this cluster:

- **Total agents** — all agents regardless of phase.
- **Working** — Running agents whose activity state is `working`.
- **Idle** — Running agents not working, plus agents in transient phases (Creating, Starting, etc.).
- **Offline** — Stopped, Failed, or Deleted agents.

These numbers come directly from agent CRD status fields and are always available regardless of TSDB configuration.

### Agent Activity Breakdown

A table showing the rate of time each agent spent in `working`, `idle`, and `paused` states over the selected time range. Derived from `kyber_agent_activity_state_seconds_total`.

Each duration cell renders as a compound, human-readable duration showing the two most-significant units, selected by magnitude — `7d 23h`, `1h 38m`, `12m 46s`, or `59.4s` for sub-minute values. This formatting is display-only: the underlying metrics payload (`/api/v1/metrics/activity`) stays in seconds. Absent values render as `—`.

Empty state: shown when Prometheus is not configured or no data exists for the selected range.

### Working Time Trend

A bar chart showing cumulative working hours per agent over the selected time range. Useful for billing attribution and identifying which agents are doing the most work. Derived from `kyber_agent_activity_state_seconds_total{state="working"}`.

### Token Usage

A table shows finalized calls and locally observed input, output, cache-write, and cache-read tokens for each agent, model, configured route, and known upstream provider. An unknown token count appears as `—`; a `*` marks a partial total when some calls did not report that class. An unknown upstream provider stays `Unknown` rather than being inferred from the model or route.

The panel reads `/api/v1/usage/events` for the exact selected time range, following its stable cursor. If more than 5,000 events match, it asks the operator to choose a shorter range instead of displaying a partial total. Detected collection gaps are shown above the table. No prices or dollar estimates are displayed.

Empty state: shown when no durable usage records are available in the selected range. Usage from before the new collection path was enabled is not backfilled from older snapshots.

### Last Active

A table listing each agent with its time since last successful heartbeat and current activity state. Agents whose last heartbeat exceeds the **staleness threshold** (default: 5 minutes, configurable via `metrics.stalenessThresholdSeconds` in Helm values) are visually distinguished with an amber row tint and a stale indicator.

This panel is backed by CRD status fields and is always available.

**Staleness threshold:** An agent is flagged stale when the control plane has not received a heartbeat from its status sidecar within the threshold. This may indicate: the pod is down, the sidecar crashed, or the agent is under heavy load and not reporting. Check agent logs or the Shell tab to investigate.

### Node Resources (live)

Live CPU, memory, and disk gauges per node. This panel refreshes automatically every 30 seconds, independent of the time range picker. It uses Prometheus instant queries.

- **CPU%** — CPU utilisation averaged across all cores.
- **Memory** — Used vs. total physical memory.
- **Disk** — Used vs. total disk space on the root filesystem.

Gauge color: green below 70%, amber 70–85%, red above 85%.

Empty state: shown when node_exporter metrics are not flowing to the TSDB.

### Agent State Change Frequency

A table showing how many times each agent transitioned into each named state over the selected time range. Derived from `kyber_agent_state_changes_total`. Useful for spotting reliability regressions (e.g., an agent bouncing between Failed and Running repeatedly).

## Empty States

Each panel renders a descriptive empty state when:

- The Prometheus URL is not configured (`metrics.prometheusURL` is empty in Helm values).
- The TSDB is unreachable (30-second timeout).
- No data exists for the selected time range (e.g., telemetry was just enabled, or the range predates data collection).
- The cluster has no agents (Fleet Summary, Last Active).

No panel crashes or spins indefinitely on data unavailability.
