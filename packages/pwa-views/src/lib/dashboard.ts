import type { Agent } from './types'

export const DEFAULT_LIST_LIMIT = 6
export const STALE_THRESHOLD_MS = 180_000 // 180s

const ATTENTION_PHASES = new Set<string>(['NeedsAuth', 'MemoryExhausted', 'Failed'])

// Canonical status-bar order: healthy/transitional first, attention last.
const PHASE_ORDER: string[] = [
  'Running', 'Starting', 'Creating', 'Restarting',
  'WaitingForMachine', 'Draining', 'Stopping', 'Stopped', 'Deleted',
  'NeedsAuth', 'MemoryExhausted', 'Failed',
]

export interface PhaseCount { phase: string; count: number }

export function phaseCounts(agents: Agent[]): PhaseCount[] {
  const counts = new Map<string, number>()
  for (const a of agents) counts.set(a.phase, (counts.get(a.phase) ?? 0) + 1)
  const out: PhaseCount[] = []
  for (const phase of PHASE_ORDER) {
    const count = counts.get(phase)
    if (count) { out.push({ phase, count }); counts.delete(phase) }
  }
  for (const phase of [...counts.keys()].sort()) out.push({ phase, count: counts.get(phase)! })
  return out
}

export function isAttentionPhase(phase: string): boolean {
  return ATTENTION_PHASES.has(phase)
}

function parseMs(ts: string | undefined): number | null {
  if (!ts) return null
  const t = Date.parse(ts)
  return Number.isNaN(t) ? null : t
}

export function isStale(agent: Agent, now: number): boolean {
  const t = parseMs(agent.activity?.lastHeartbeatAt)
  return t !== null && now - t > STALE_THRESHOLD_MS
}

// Most-recent activity epoch (lastActivityAt, else lastHeartbeatAt); -Infinity if none.
export function activityAt(agent: Agent): number {
  return parseMs(agent.activity?.lastActivityAt ?? agent.activity?.lastHeartbeatAt) ?? -Infinity
}

export function rankByActivity(agents: Agent[], limit: number): Agent[] {
  return agents
    .filter((a) => activityAt(a) > -Infinity)
    .slice()
    .sort((x, y) => activityAt(y) - activityAt(x))
    .slice(0, limit)
}

export interface ContextRow {
  agent: Agent; percentage: number; used: number; limit: number; model: string
}

export function rankByContext(agents: Agent[], limit: number): ContextRow[] {
  return agents
    .filter((a) => a.tokenUsage && a.tokenUsage.contextWindowKnown !== false)
    .map((a) => ({
      agent: a, percentage: a.tokenUsage!.percentage,
      used: a.tokenUsage!.tokens.used, limit: a.tokenUsage!.tokens.limit,
      model: a.tokenUsage!.model,
    }))
    .sort((x, y) => y.percentage - x.percentage)
    .slice(0, limit)
}

export type ContextTone = 'success' | 'warn' | 'danger'
export function contextTone(pct: number): ContextTone {
  if (pct > 85) return 'danger'
  if (pct >= 60) return 'warn'
  return 'success'
}

export function formatAgo(ts: string | undefined, now: number): string {
  const t = parseMs(ts)
  if (t === null) return '—'
  const s = Math.max(0, Math.round((now - t) / 1000))
  if (s < 60) return `${s}s ago`
  const m = Math.round(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.round(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.round(h / 24)}d ago`
}

export function formatTokens(n: number): string {
  if (n >= 1_000_000) {
    const m = n / 1_000_000
    return `${Number.isInteger(m) ? m : m.toFixed(1)}M`
  }
  if (n >= 1000) return `${Math.round(n / 1000)}k`
  return `${n}`
}
