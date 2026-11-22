import { projectWork, workLabels, workReasons } from '../lib/activity'
import { useActivityClock } from '../hooks/useActivityClock'
import type { Agent } from '../lib/types'

export interface AgentActivityDotProps {
  agent: Agent
}

// All activity surfaces share the lifecycle/freshness projection. Legacy
// state is diagnostic history and cannot keep a working dot alive.
export function AgentActivityDot({ agent }: AgentActivityDotProps) {
  const now = useActivityClock()
  const work = projectWork(agent, now)
  if (work.state === 'excluded') return null
  const color = work.state === 'working' ? 'bg-success' : work.state === 'background' ? 'bg-info' : work.state === 'waiting' ? 'bg-text-disabled' : 'bg-warn'
  return <span role="status" aria-label={workLabels[work.state]} data-testid="agent-activity-dot" data-state={work.state} title={work.state === 'unavailable' ? workReasons[work.reason ?? ''] ?? 'Activity unavailable' : workLabels[work.state]} className={`inline-block h-2 w-2 rounded-full ${color}`} />
}

/**
 * formatRelativeTime — "30s ago", "5m ago", "2h ago", "1d ago". Same
 * shape as SchedulingFailureBanner's formatObservedAgo (kyber#210
 * PR-B); replicated here rather than imported to avoid pulling in
 * unrelated banner dependencies just for the helper. If a third
 * caller needs this, factor into pwa/src/lib/relativeTime.ts.
 */
export function formatRelativeTime(iso: string, now: Date = new Date()): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return 'recently'
  const diffSec = Math.max(0, Math.floor((now.getTime() - t) / 1000))
  if (diffSec < 60) return `${diffSec}s ago`
  const diffMin = Math.floor(diffSec / 60)
  if (diffMin < 60) return `${diffMin}m ago`
  const diffHr = Math.floor(diffMin / 60)
  if (diffHr < 24) return `${diffHr}h ago`
  return `${Math.floor(diffHr / 24)}d ago`
}
