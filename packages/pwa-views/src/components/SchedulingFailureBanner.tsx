import { AlertTriangle } from 'lucide-react'
import type { Agent, AgentSchedulingCategory } from '../lib/types'
import { DiagnosticDetails } from './DiagnosticDetails'

export interface SchedulingFailureBannerProps {
  agent: Agent
}

interface CategoryCopy {
  headline: string
  /**
   * Per-category remediation paragraph. The `${machine}` token is replaced
   * with `agent.machine`. Categories where remediation is generic (Other)
   * leave this blank — only the verbatim message renders.
   */
  remediation: (machine: string) => string | null
}

const CATEGORY_COPY: Record<AgentSchedulingCategory, CategoryCopy> = {
  Capacity: {
    headline: "Pod can't schedule — capacity full",
    remediation: (machine) =>
      `${machine} doesn't have enough capacity for this agent's request. Reduce the request, delete another agent on the same machine, or pick a different machine.`,
  },
  Placement: {
    headline: "Pod can't schedule — no matching node",
    remediation: () =>
      "No node matches this agent's affinity / taint requirements. Adjust scheduling constraints on the agent or the node.",
  },
  Image: {
    headline: 'Container image pull failed',
    remediation: () =>
      "The agent's image couldn't be pulled. Check that the image exists in the registry and that the cluster's image-pull credentials are wired.",
  },
  Storage: {
    headline: 'Volume binding failed',
    remediation: () =>
      "The agent's persistent volume couldn't bind. Check the storage class is available and the requested size fits.",
  },
  Other: {
    headline: 'Pod scheduling failure',
    remediation: () => null,
  },
}

/**
 * SchedulingFailureBanner — surfaces an `Agent.status.scheduling` entry
 * (populated by the controller after a 30s grace window when a Pod event
 * matches a known scheduling/kubelet failure reason). Renders nothing
 * when the agent has no scheduling failure recorded.
 *
 * Copy is keyed off `scheduling.category`; the verbatim
 * scheduler/kubelet message is rendered in mono so operators have an
 * exact search string for kubectl / Stack Overflow.
 *
 * Spec: kyber#210 PR-B.
 */
export function SchedulingFailureBanner({ agent }: SchedulingFailureBannerProps) {
  const sched = agent.scheduling
  const waitingForMachine = agent.phase === 'WaitingForMachine'
  if (!sched && !waitingForMachine) return null

  if (waitingForMachine) {
    const details = [
      `agent: ${agent.id}`,
      `machine: ${agent.machine}`,
      `phase: ${agent.phase}`,
      agent.status.message ? `message: ${agent.status.message}` : '',
      sched?.lastError ? `scheduler: ${sched.lastError}` : '',
    ].filter(Boolean).join('\n')
    return (
      <div role="status" data-testid="machine-wait-banner" className="rounded-lg border border-accent/40 bg-accent/10 p-4">
        <div className="flex items-start gap-3">
          <AlertTriangle className="mt-0.5 h-5 w-5 shrink-0 text-accent" aria-hidden="true" />
          <div className="min-w-0 flex-1 space-y-2">
            <h3 className="text-sm font-semibold text-text-primary">Waiting for machine capacity</h3>
            <p className="text-xs text-text-secondary">
              Machine {agent.machine} is recovering. Kyber will restart this agent automatically when a Ready node joins; no operator action is required.
            </p>
            {details && <DiagnosticDetails details={details} />}
          </div>
        </div>
      </div>
    )
  }

  if (!sched) return null

  const copy = CATEGORY_COPY[sched.category] ?? CATEGORY_COPY.Other
  const remediation = copy.remediation(agent.machine)
  const observedAgo = sched.firstObservedAt ? formatObservedAgo(sched.firstObservedAt) : null

  return (
    <div
      role="alert"
      data-testid="scheduling-failure-banner"
      data-category={sched.category}
      className="rounded-lg border border-warn/40 bg-warn-muted p-4"
    >
      <div className="flex items-start gap-3">
        <AlertTriangle className="h-5 w-5 shrink-0 text-warn mt-0.5" aria-hidden="true" />
        <div className="min-w-0 flex-1 space-y-2">
          <h3 className="text-sm font-semibold text-warn">{copy.headline}</h3>
          {remediation && (
            <p className="text-xs text-warn/90">{remediation}</p>
          )}
          {sched.lastError && <DiagnosticDetails details={sched.lastError} testId="scheduling-failure-message" />}
          <p className="text-[11px] text-warn/70">
            {observedAgo ? `First observed ${observedAgo} · ` : ''}
            category: {sched.category}
          </p>
        </div>
      </div>
    </div>
  )
}

/**
 * formatObservedAgo — "5 min ago", "2 h ago", "just now". Operators read
 * this to gauge whether the failure is stale-but-not-cleared or actively
 * happening; precision past minutes/hours doesn't add value.
 */
export function formatObservedAgo(iso: string, now: Date = new Date()): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return 'recently'
  const diffSec = Math.max(0, Math.floor((now.getTime() - t) / 1000))
  if (diffSec < 30) return 'just now'
  if (diffSec < 60) return `${diffSec} sec ago`
  const diffMin = Math.floor(diffSec / 60)
  if (diffMin < 60) return `${diffMin} min ago`
  const diffHr = Math.floor(diffMin / 60)
  if (diffHr < 24) return `${diffHr} h ago`
  const diffDay = Math.floor(diffHr / 24)
  return `${diffDay} d ago`
}
