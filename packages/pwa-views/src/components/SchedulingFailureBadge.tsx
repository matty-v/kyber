import { AlertTriangle } from 'lucide-react'
import type { Agent } from '../lib/types'

export interface SchedulingFailureBadgeProps {
  agent: Agent
}

/**
 * SchedulingFailureBadge — small inline warning badge shown next to an
 * agent's StatusBadge when the controller has populated
 * `Agent.status.scheduling`. Tooltip surfaces the verbatim
 * scheduler/kubelet message so an operator can hover-then-decide
 * without opening the detail page.
 *
 * Renders nothing when there's no scheduling failure recorded. Spec:
 * kyber#210 PR-B.
 */
export function SchedulingFailureBadge({ agent }: SchedulingFailureBadgeProps) {
  const sched = agent.scheduling
  if (!sched) return null

  // Tooltip = category + verbatim message. Operators trust the verbatim
  // string more than any rewording, and `title` is the universal
  // tooltip surface across desktop + mobile (long-press on iOS).
  const tooltip = sched.lastError
    ? `${sched.category}: ${sched.lastError}`
    : `Scheduling failure: ${sched.category}`

  return (
    <span
      role="status"
      aria-label={`Scheduling failure: ${sched.category}`}
      data-testid="scheduling-failure-badge"
      data-category={sched.category}
      title={tooltip}
      className="inline-flex items-center gap-1 rounded-full bg-warn-muted px-1.5 py-0.5 text-[10px] font-medium text-warn"
    >
      <AlertTriangle className="h-3 w-3" aria-hidden="true" />
      {sched.category}
    </span>
  )
}
