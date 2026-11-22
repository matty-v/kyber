import type { Agent } from '../lib/types'
import { projectWork, workLabels, workReasons } from '../lib/activity'
import { useActivityClock } from '../hooks/useActivityClock'

export interface AgentActivityBadgeProps { agent: Agent; showDot?: boolean }

export function AgentActivityBadge({ agent, showDot = true }: AgentActivityBadgeProps) {
  const now = useActivityClock()
  const work = projectWork(agent, now)
  if (work.state === 'excluded') return null
  const text = work.state === 'background' ? `${work.backgroundCount} ${work.backgroundCount === 1 ? 'shell' : 'shells'} running` : workLabels[work.state]
  const color = work.state === 'working' ? 'text-success' : work.state === 'background' ? 'text-info' : work.state === 'unavailable' ? 'text-warn' : 'text-text-secondary'
  return <span data-testid="agent-activity-badge" data-state={work.state} title={work.state === 'unavailable' ? workReasons[work.reason ?? ''] : undefined} className={`inline-flex items-center gap-1.5 whitespace-nowrap rounded-full bg-surface-overlay px-2 py-0.5 text-xs font-medium ${color}`}>
    {showDot && <span aria-hidden className="inline-block h-1.5 w-1.5 rounded-full bg-current" />}{text}
  </span>
}
