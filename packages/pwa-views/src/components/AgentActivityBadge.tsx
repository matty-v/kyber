import type { Agent } from '../lib/types'
import { formatRelativeTime } from './AgentActivityDot'

export interface AgentActivityBadgeProps {
  agent: Agent
}

/**
 * AgentActivityBadge — text-form activity indicator for AgentDetail.
 * Shows "Working" or "Idle <relative-time>" based on
 * agent.status.activity. Renders nothing when the runtime hasn't
 * reported a state yet (same visible-by-absence policy as
 * AgentActivityDot).
 *
 * Spec: kyber#249 (closes kyber#174).
 */
export function AgentActivityBadge({ agent }: AgentActivityBadgeProps) {
  const a = agent.activity
  if (!a || !a.state || a.state === 'unknown') {
    return null
  }
  const working = a.state === 'working'
  const text = working
    ? 'Working'
    : a.lastActivityAt
      ? `Idle ${formatRelativeTime(a.lastActivityAt)}`
      : 'Idle'

  return (
    <span
      data-testid="agent-activity-badge"
      data-state={a.state}
      className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium ${
        working
          ? 'bg-success/15 text-success'
          : 'bg-surface-overlay text-text-secondary'
      }`}
    >
      <span
        className={`inline-block h-1.5 w-1.5 rounded-full ${
          working ? 'bg-success motion-safe:animate-pulse' : 'bg-text-disabled'
        }`}
        aria-hidden="true"
      />
      {text}
    </span>
  )
}
