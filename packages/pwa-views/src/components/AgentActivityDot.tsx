import type { Agent } from '../lib/types'

export interface AgentActivityDotProps {
  agent: Agent
}

/**
 * AgentActivityDot — small inline dot rendered next to a StatusBadge
 * to convey whether the agent is mid-turn (`working`) vs waiting for
 * input (`idle`). Renders nothing when the runtime hasn't reported a
 * state yet — visible-by-absence beats faking a stale state.
 *
 * Spec: kyber#249 (closes kyber#174). Source: in-pod
 * kyber-token-reporter pushes activity events to
 * /internal/agents/{name}/status-event; control plane patches
 * Agent.status.activity.{state, lastActivityAt}; this component renders
 * the result.
 *
 * Tooltip on hover surfaces the verbatim state + relative-time
 * lastActivityAt for operators who want the precise reading.
 */
export function AgentActivityDot({ agent }: AgentActivityDotProps) {
  const a = agent.activity
  if (!a || !a.state || a.state === 'unknown') {
    return null
  }
  const working = a.state === 'working'
  const colorClass = working ? 'bg-success' : 'bg-text-disabled'
  const pulseClass = working ? 'motion-safe:animate-pulse' : ''
  const title = working
    ? 'Working'
    : a.lastActivityAt
      ? `Idle ${formatRelativeTime(a.lastActivityAt)}`
      : 'Idle'

  return (
    <span
      role="status"
      aria-label={working ? 'Agent is working' : 'Agent is idle'}
      data-testid="agent-activity-dot"
      data-state={a.state}
      title={title}
      className={`inline-block h-2 w-2 rounded-full ${colorClass} ${pulseClass}`}
    />
  )
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
