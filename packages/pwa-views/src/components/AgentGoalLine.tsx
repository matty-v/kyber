import type { AgentGoalStatus } from '../lib/types'

export function AgentGoalLine({ goal, className = '' }: { goal?: AgentGoalStatus; className?: string }) {
  if (!goal?.summary) return null

  const tone = goal.source === 'platform' ? 'text-text-muted' : 'text-text-secondary'
  return (
    <p
      className={`block min-w-0 max-w-full truncate text-xs ${tone} ${className}`}
      title={goal.summary}
      data-testid="agent-goal"
    >
      {goal.summary}
    </p>
  )
}
