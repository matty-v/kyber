import { AlertTriangle } from 'lucide-react'
import type { AgentResourceUsage as ResourceUsage } from '../lib/types'

function percent(used: number, total: number | undefined): number | null {
  if (!total || total <= 0) return null
  return Math.max(0, (used / total) * 100)
}

function formatBytes(value: number): string {
  const gib = value / 1024 ** 3
  if (gib >= 0.1) return `${gib.toFixed(1)} GiB`
  return `${(value / 1024 ** 2).toFixed(0)} MiB`
}

function ResourceRow({ label, used, total, display }: { label: string; used: number; total?: number; display: string }) {
  const pct = percent(used, total)
  const color = pct !== null && pct >= 90 ? 'bg-danger' : pct !== null && pct >= 80 ? 'bg-warn' : 'bg-success'
  return (
    <div data-testid={`resource-${label.toLowerCase()}`}>
      <div className="mb-1 flex justify-between gap-3 text-xs">
        <span className="text-text-muted">{label}</span>
        <span className={`font-mono tabular-nums ${pct !== null && pct >= 90 ? 'text-danger' : pct !== null && pct >= 80 ? 'text-warn' : 'text-text-primary'}`}>
          {display}
        </span>
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-surface-overlay">
        <div className={`h-full ${color}`} style={{ width: `${Math.min(100, pct ?? 0)}%` }} data-usage-band={pct !== null && pct >= 90 ? 'danger' : pct !== null && pct >= 80 ? 'warn' : 'normal'} />
      </div>
    </div>
  )
}

export function AgentResourceUsage({ usage }: { usage: ResourceUsage }) {
  const cpuTotal = usage.cpuLimitCores
  const memoryTotal = usage.memoryLimitBytes
  return (
    <div className="space-y-3" aria-label="Current resource usage">
      <ResourceRow label="CPU" used={usage.cpuUsageCores} total={cpuTotal} display={`${usage.cpuUsageCores.toFixed(2)} of ${cpuTotal?.toFixed(2) ?? 'unlimited'} cores`} />
      <ResourceRow label="Memory" used={usage.memoryUsedBytes} total={memoryTotal} display={`${formatBytes(usage.memoryUsedBytes)} of ${memoryTotal ? formatBytes(memoryTotal) : 'unlimited'}`} />
      <ResourceRow label="Disk" used={usage.diskUsedBytes} total={usage.diskTotalBytes} display={`${formatBytes(usage.diskUsedBytes)} of ${formatBytes(usage.diskTotalBytes)}`} />
    </div>
  )
}

export function AgentDiskPressureBadge({ usage }: { usage?: ResourceUsage }) {
  const pct = usage ? percent(usage.diskUsedBytes, usage.diskTotalBytes) : null
  if (pct === null || pct < 80) return null
  return (
    <span className={`inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium ${pct >= 90 ? 'bg-danger/15 text-danger' : 'bg-warn/15 text-warn'}`} title={`Persistent disk is ${pct.toFixed(1)}% full`}>
      <AlertTriangle className="h-3 w-3" /> Disk {pct.toFixed(0)}%
    </span>
  )
}
