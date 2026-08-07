import { useState, useMemo } from 'react'
import { BarChart3, Wifi, WifiOff } from 'lucide-react'
import { Card } from '../components/Card'
import { Skeleton } from '../components/Skeleton'
import { EmptyState } from '../components/EmptyState'
import {
  useMetricsSummary,
  useMetricsActivity,
  useMetricsWorkingTime,
  useMetricsTokens,
  useMetricsLastActive,
  useMetricsNodes,
  useMetricsStateChanges,
} from '../hooks/useAPI'
import { formatDuration } from '../lib/duration'
import type {
  MetricsFleetSummary,
  MetricsSeries,
  MetricsTokenUsage,
  MetricsLastActive,
  MetricsNodeResources,
  MetricsStateChange,
} from '../lib/types'

// ---- Time range picker ----

type RangeOption = { label: string; seconds: number }

const RANGE_OPTIONS: RangeOption[] = [
  { label: 'Last 15 min', seconds: 15 * 60 },
  { label: '1 h', seconds: 60 * 60 },
  { label: '6 h', seconds: 6 * 60 * 60 },
  { label: '24 h', seconds: 24 * 60 * 60 },
  { label: '7 d', seconds: 7 * 24 * 60 * 60 },
]

function TimeRangePicker({
  selected,
  onChange,
}: {
  selected: RangeOption
  onChange: (r: RangeOption) => void
}) {
  return (
    <div className="flex gap-1">
      {RANGE_OPTIONS.map((r) => (
        <button
          key={r.label}
          onClick={() => onChange(r)}
          className={`rounded-md px-3 py-1.5 text-xs font-medium transition-colors ${
            r === selected
              ? 'bg-accent text-white'
              : 'bg-surface-overlay text-text-muted hover:text-text-primary'
          }`}
        >
          {r.label}
        </button>
      ))}
    </div>
  )
}

// ---- Stat card ----

function StatCard({ label, value }: { label: string; value: number }) {
  return (
    <Card>
      <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted">{label}</p>
      <p className="mt-2 font-display text-4xl font-semibold tabular-nums text-text-primary">{value}</p>
    </Card>
  )
}

// ---- Panel wrapper ----

function Panel({
  title,
  children,
  className = '',
}: {
  title: string
  children: React.ReactNode
  className?: string
}) {
  return (
    <div className={`rounded-xl border border-border-subtle bg-surface-raised p-4 ${className}`}>
      <p className="mb-3 font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted">{title}</p>
      {children}
    </div>
  )
}

function PanelEmpty({ message }: { message: string }) {
  return (
    <p className="py-6 text-center text-sm text-text-disabled">{message}</p>
  )
}

// ---- Fleet Summary panel ----

function FleetSummaryPanel() {
  const { data, isLoading } = useMetricsSummary()

  if (isLoading) {
    return (
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-20 rounded-xl border border-border-subtle" />
        ))}
      </div>
    )
  }

  const summary: MetricsFleetSummary = data ?? { total: 0, working: 0, idle: 0, offline: 0 }

  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
      <StatCard label="Total agents" value={summary.total} />
      <StatCard label="Working" value={summary.working} />
      <StatCard label="Idle" value={summary.idle} />
      <StatCard label="Offline" value={summary.offline} />
    </div>
  )
}

// ---- Agent Activity panel ----

function AgentActivityPanel({ start, end }: { start: number; end: number }) {
  const { data, isLoading } = useMetricsActivity(start, end)

  return (
    <Panel title="Agent Activity Breakdown">
      {isLoading && <Skeleton className="h-24" />}
      {!isLoading && (!data || data.length === 0) && (
        <PanelEmpty message="No activity data for this time range. Ensure telemetry is enabled on this cluster." />
      )}
      {!isLoading && data && data.length > 0 && (
        <ActivitySeriesTable series={data} />
      )}
    </Panel>
  )
}

// ACTIVITY_STATES is the column set rendered in AGENT ACTIVITY BREAKDOWN.
// "unknown" was added in kyber#360 Cause F — the runtime emits it on
// activity-detector errors (pkg/tokenreport/activity.go:ActivityUnknown)
// and the CP now accepts and stores it; the panel must render it so the
// stack is non-broken when one appears. Rendered with muted text since
// "unknown" is operator-confused state, not active work.
const ACTIVITY_STATES = ['working', 'idle', 'paused', 'unknown'] as const

function ActivitySeriesTable({ series }: { series: MetricsSeries[] }) {
  const byAgent = useMemo(() => {
    const m: Record<string, Record<string, number>> = {}
    for (const s of series) {
      const agent = s.labels['agent'] ?? '(unknown)'
      const state = s.labels['state'] ?? 'unknown'
      if (!m[agent]) m[agent] = {}
      const total = s.points.reduce((sum, p) => sum + p.v, 0)
      m[agent][state] = (m[agent][state] ?? 0) + total
    }
    return m
  }, [series])

  const agents = Object.keys(byAgent).sort()

  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-border-subtle text-left text-text-muted">
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">Agent</th>
            {ACTIVITY_STATES.map((s) => (
              <th key={s} className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">{s}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {agents.map((agent) => (
            <tr key={agent} className="border-b border-border-subtle/40">
              <td className="py-2 pr-4 font-mono text-text-primary">{agent}</td>
              {ACTIVITY_STATES.map((s) => (
                <td
                  key={s}
                  className={`py-2 pr-4 tabular-nums ${s === 'unknown' ? 'text-text-disabled' : 'text-text-secondary'}`}
                >
                  {byAgent[agent][s] != null ? formatDuration(byAgent[agent][s]) : '—'}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// ---- Working Time Trend panel ----

function WorkingTimeTrendPanel({ start, end }: { start: number; end: number }) {
  const { data, isLoading } = useMetricsWorkingTime(start, end)

  return (
    <Panel title="Working Time Trend">
      {isLoading && <Skeleton className="h-24" />}
      {!isLoading && (!data || data.length === 0) && (
        <PanelEmpty message="No working time data for this time range." />
      )}
      {!isLoading && data && data.length > 0 && (
        <WorkingTimeTable series={data} />
      )}
    </Panel>
  )
}

function WorkingTimeTable({ series }: { series: MetricsSeries[] }) {
  const byAgent = useMemo(() => {
    const m: Record<string, number> = {}
    for (const s of series) {
      const agent = s.labels['agent'] ?? '(unknown)'
      const total = s.points.reduce((sum, p) => sum + p.v, 0)
      m[agent] = (m[agent] ?? 0) + total
    }
    return m
  }, [series])

  const entries = Object.entries(byAgent).sort((a, b) => b[1] - a[1])

  return (
    <div className="space-y-2">
      {entries.map(([agent, hours]) => (
        <div key={agent} className="flex items-center gap-3">
          <span className="w-32 truncate font-mono text-xs text-text-primary">{agent}</span>
          <div className="flex-1 rounded-full bg-surface-base h-2 overflow-hidden">
            <div
              className="h-full rounded-full bg-accent"
              style={{ width: `${Math.min(100, (hours / (entries[0][1] || 1)) * 100)}%` }}
            />
          </div>
          <span className="w-16 text-right font-mono text-xs tabular-nums text-text-secondary">
            {hours.toFixed(2)}h
          </span>
        </div>
      ))}
    </div>
  )
}

// ---- Token Usage panel ----

function TokenUsagePanel({ start, end }: { start: number; end: number }) {
  const { data, isLoading } = useMetricsTokens(start, end)

  return (
    <Panel title="Token Usage and Cost">
      {isLoading && <Skeleton className="h-32" />}
      {!isLoading && (!data || data.length === 0) && (
        <PanelEmpty message="No token usage recorded yet for this cluster in the selected time range." />
      )}
      {!isLoading && data && data.length > 0 && (
        <TokenTable rows={data} />
      )}
    </Panel>
  )
}

// An explicit priced===false is "unpriced" (no rate in provider-rates);
// older payloads omit the field and are treated as priced, same defensive
// check as windowUnknown in TokenUsage.tsx (kyber#487).
function isUnpriced(row: MetricsTokenUsage): boolean {
  return row.priced === false
}

export function TokenTable({ rows }: { rows: MetricsTokenUsage[] }) {
  // Priced rows by cost desc; unpriced rows sorted last (their costUsd is a
  // placeholder 0, so we can't let it rank among genuine $0 rows).
  const sorted = [...rows].sort((a, b) => {
    const au = isUnpriced(a)
    const bu = isUnpriced(b)
    if (au !== bu) return au ? 1 : -1
    return b.costUsd - a.costUsd
  })
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-border-subtle text-left text-text-muted">
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">Agent</th>
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">Model</th>
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">Input</th>
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">Output</th>
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">Cache read</th>
            <th className="pb-2 font-mono font-medium uppercase tracking-wider">Cost (USD)</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((row, i) => (
            <tr key={i} className="border-b border-border-subtle/40">
              <td className="py-2 pr-4 font-mono text-text-primary">{row.agent}</td>
              <td className="py-2 pr-4 text-text-secondary">{row.model}</td>
              <td className="py-2 pr-4 tabular-nums text-text-secondary">{(row.tokens['input'] ?? 0).toLocaleString()}</td>
              <td className="py-2 pr-4 tabular-nums text-text-secondary">{(row.tokens['output'] ?? 0).toLocaleString()}</td>
              <td className="py-2 pr-4 tabular-nums text-text-secondary">{(row.tokens['cache_read'] ?? 0).toLocaleString()}</td>
              {isUnpriced(row) ? (
                <td className="py-2 tabular-nums">
                  <span className="text-text-muted">—</span>
                  <span
                    className="ml-1 italic text-text-muted"
                    title="No rate for this model in provider-rates — add it to fix the cost."
                  >
                    · unpriced
                  </span>
                </td>
              ) : (
                <td className="py-2 tabular-nums text-text-primary font-medium">${row.costUsd.toFixed(4)}</td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// ---- Last Active panel ----

function LastActivePanel() {
  const { data, isLoading } = useMetricsLastActive()

  return (
    <Panel title="Last Active">
      {isLoading && <Skeleton className="h-32" />}
      {!isLoading && (!data || data.length === 0) && (
        <PanelEmpty message="No agents found." />
      )}
      {!isLoading && data && data.length > 0 && (
        <LastActiveTable rows={data} />
      )}
    </Panel>
  )
}

function LastActiveTable({ rows }: { rows: MetricsLastActive[] }) {
  const sorted = [...rows].sort((a, b) => {
    const sa = a.secondsSinceHeartbeat ?? Infinity
    const sb = b.secondsSinceHeartbeat ?? Infinity
    return sa - sb
  })

  function formatAge(secs?: number): string {
    if (secs == null) return 'never'
    if (secs < 60) return `${secs}s ago`
    if (secs < 3600) return `${Math.floor(secs / 60)}m ago`
    return `${Math.floor(secs / 3600)}h ago`
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-border-subtle text-left text-text-muted">
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">Agent</th>
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">State</th>
            <th className="pb-2 font-mono font-medium uppercase tracking-wider">Last heartbeat</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((row) => (
            <tr
              key={row.name}
              className={`border-b border-border-subtle/40 ${row.stale ? 'bg-warning-muted/30' : ''}`}
            >
              <td className="py-2 pr-4 font-mono text-text-primary">{row.name}</td>
              <td className="py-2 pr-4 text-text-secondary capitalize">{row.state}</td>
              <td className={`py-2 font-mono tabular-nums ${row.stale ? 'text-warning' : 'text-text-secondary'}`}>
                {formatAge(row.secondsSinceHeartbeat)}
                {row.stale && (
                  <span className="ml-2 inline-flex items-center gap-1">
                    <WifiOff className="h-3 w-3" />
                    <span>stale</span>
                  </span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// ---- Node Resources panel ----

function NodeResourcesPanel() {
  const { data, isLoading } = useMetricsNodes()

  return (
    <Panel title="Node Resources (live)">
      {isLoading && <Skeleton className="h-24" />}
      {!isLoading && (!data || data.length === 0) && (
        <PanelEmpty message="No node resource data available. Node agents may not be running or have not reported yet." />
      )}
      {!isLoading && data && data.length > 0 && (
        <NodeResourcesTable rows={data} />
      )}
    </Panel>
  )
}

function formatBytes(b: number): string {
  if (b >= 1e9) return `${(b / 1e9).toFixed(1)} GB`
  if (b >= 1e6) return `${(b / 1e6).toFixed(1)} MB`
  return `${Math.round(b / 1e3)} KB`
}

function GaugeBar({ used, total }: { used: number; total: number }) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0
  const color = pct > 85 ? 'bg-danger' : pct > 70 ? 'bg-warning' : 'bg-accent'
  return (
    <div className="flex items-center gap-2">
      <div className="flex-1 h-1.5 rounded-full bg-surface-base overflow-hidden">
        <div className={`h-full rounded-full ${color}`} style={{ width: `${pct}%` }} />
      </div>
      <span className="w-8 text-right font-mono text-xs tabular-nums text-text-secondary">{pct.toFixed(0)}%</span>
    </div>
  )
}

function NodeResourcesTable({ rows }: { rows: MetricsNodeResources[] }) {
  return (
    <div className="space-y-4">
      {rows.map((row) => (
        <div key={row.node}>
          <p className="mb-2 font-mono text-xs font-medium text-text-primary">{row.node}</p>
          <div className="space-y-1.5">
            <div className="flex items-center gap-3 text-xs text-text-muted">
              <span className="w-10">CPU</span>
              <GaugeBar used={row.cpuPercent} total={100} />
            </div>
            <div className="flex items-center gap-3 text-xs text-text-muted">
              <span className="w-10">Mem</span>
              <GaugeBar used={row.memUsedBytes} total={row.memTotalBytes} />
              <span className="text-text-disabled">{formatBytes(row.memUsedBytes)} / {formatBytes(row.memTotalBytes)}</span>
            </div>
            <div className="flex items-center gap-3 text-xs text-text-muted">
              <span className="w-10">Disk</span>
              <GaugeBar used={row.diskUsedBytes} total={row.diskTotalBytes} />
              <span className="text-text-disabled">{formatBytes(row.diskUsedBytes)} / {formatBytes(row.diskTotalBytes)}</span>
            </div>
          </div>
        </div>
      ))}
    </div>
  )
}

// ---- State Changes panel ----

function StateChangesPanel({ start, end }: { start: number; end: number }) {
  const { data, isLoading } = useMetricsStateChanges(start, end)

  return (
    <Panel title="Agent State Change Frequency">
      {isLoading && <Skeleton className="h-24" />}
      {!isLoading && (!data || data.length === 0) && (
        <PanelEmpty message="No state change data for this time range." />
      )}
      {!isLoading && data && data.length > 0 && (
        <StateChangesTable rows={data} />
      )}
    </Panel>
  )
}

function StateChangesTable({ rows }: { rows: MetricsStateChange[] }) {
  const sorted = [...rows].sort((a, b) => b.count - a.count)
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-border-subtle text-left text-text-muted">
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">Agent</th>
            <th className="pb-2 pr-4 font-mono font-medium uppercase tracking-wider">→ State</th>
            <th className="pb-2 font-mono font-medium uppercase tracking-wider">Count</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((row, i) => (
            <tr key={i} className="border-b border-border-subtle/40">
              <td className="py-2 pr-4 font-mono text-text-primary">{row.agent}</td>
              <td className="py-2 pr-4 text-text-secondary">{row.toState}</td>
              <td className="py-2 tabular-nums text-text-primary font-medium">{Math.round(row.count)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// ---- MetricsTab root ----

export function MetricsTab() {
  const [range, setRange] = useState<RangeOption>(RANGE_OPTIONS[1]) // default: 1h

  const { start, end } = useMemo(() => {
    const now = Math.floor(Date.now() / 1000)
    return { start: now - range.seconds, end: now }
  }, [range])

  return (
    <div>
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-bold text-text-primary">Metrics</h1>
        <TimeRangePicker selected={range} onChange={setRange} />
      </div>

      <div className="space-y-6">
        <FleetSummaryPanel />

        <div className="grid gap-4 lg:grid-cols-2">
          <AgentActivityPanel start={start} end={end} />
          <WorkingTimeTrendPanel start={start} end={end} />
        </div>

        <TokenUsagePanel start={start} end={end} />

        <div className="grid gap-4 lg:grid-cols-2">
          <LastActivePanel />
          <NodeResourcesPanel />
        </div>

        <StateChangesPanel start={start} end={end} />
      </div>
    </div>
  )
}
