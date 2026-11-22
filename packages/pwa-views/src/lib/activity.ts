import type { Agent, AgentWorkView } from './types'

export const ACTIVITY_GROUPS = ['working', 'background', 'waiting', 'unavailable'] as const
export type ActivityGroup = typeof ACTIVITY_GROUPS[number]
export type WorkState = ActivityGroup | 'excluded'
export type WorkView = { state: WorkState; reason?: string; foregroundCount: number; backgroundCount: number; needsInput: boolean; lastWorkedAt?: string; observedAt?: string; stateSince?: string }
const receipts = new WeakMap<AgentWorkView, number>()

// Call when a response arrives, not on first render: a hidden tab must not
// extend a cached lease. performance.now avoids wall-clock changes after receipt.
export function recordActivityReceipt(agents: Agent[], receivedAt = performance.now()): Agent[] {
  for (const a of agents) if (a.activity?.work && !receipts.has(a.activity.work)) receipts.set(a.activity.work, receivedAt)
  return agents
}

export function activityEligible(agent: Agent): boolean {
  return agent.phase === 'Running' && (!agent.desiredPhase || agent.desiredPhase === 'Running')
}

export function projectWork(agent: Agent, monotonicNow = performance.now()): WorkView {
  const unavailable = (reason: string): WorkView => ({ state: 'unavailable', reason, foregroundCount: 0, backgroundCount: 0, needsInput: false })
  if (!activityEligible(agent)) return { ...unavailable('lifecycle'), state: 'excluded' }
  const work = agent.activity?.work
  if (!work) return unavailable('legacy_reporting')
  const received = receipts.get(work)
  if (received === undefined) return unavailable('observation_unanchored')
  const serverTime = Date.parse(work.serverTime)
  const expires = Date.parse(work.expiresAt ?? '')
  if (!Number.isFinite(serverTime) || monotonicNow < received) return unavailable('observation_invalid')
  if (work.state === 'unavailable' || work.state === 'excluded') return { ...unavailable(work.reason ?? 'reporter_stale'), state: work.state }
  if (!ACTIVITY_GROUPS.includes(work.state) || !Number.isFinite(expires) || serverTime + monotonicNow - received >= expires) return unavailable('reporter_stale')
  if (![work.foregroundCount, work.backgroundCount].every(n => Number.isInteger(n) && n >= 0 && n <= 1024)) return unavailable('observation_invalid')
  if ((work.state === 'working' && work.foregroundCount === 0) || (work.state === 'background' && (work.foregroundCount !== 0 || work.backgroundCount === 0)) || (work.state === 'waiting' && (work.foregroundCount !== 0 || work.backgroundCount !== 0))) return unavailable('observation_invalid')
  return { state: work.state, reason: work.reason, foregroundCount: work.foregroundCount, backgroundCount: work.backgroundCount, needsInput: work.needsInput === true, lastWorkedAt: work.lastWorkedAt, observedAt: work.observedAt, stateSince: work.stateSince }
}

export function activityGroups(agents: Agent[], monotonicNow = performance.now()): Record<ActivityGroup, Agent[]> {
  const groups: Record<ActivityGroup, Agent[]> = { working: [], background: [], waiting: [], unavailable: [] }
  for (const agent of agents) {
    const work = projectWork(agent, monotonicNow)
    if (work.state !== 'excluded') groups[work.state].push(agent)
  }
  for (const group of ACTIVITY_GROUPS) groups[group].sort((a,b) => {
    // Lease renewals are deliberately not the sort key.
    const key = (x: Agent) => Date.parse(x.activity?.work?.stateSince ?? x.activity?.work?.lastWorkedAt ?? '') || 0
    return key(b) - key(a) || a.id.localeCompare(b.id)
  })
  return groups
}

export const workLabels: Record<WorkState, string> = { working: 'Working', background: 'Background work', waiting: 'Waiting', unavailable: 'Activity unavailable', excluded: '' }
export const workReasons: Record<string, string> = { legacy_reporting: 'Legacy reporting', reporter_stale: 'No recent activity signal', contact_stale: 'Last contact is stale', contact_missing: 'No contact recorded', inventory_incomplete: 'Activity coverage incomplete', observation_unanchored: 'Activity not yet verified', observation_invalid: 'Invalid activity signal', inventory_invalid: 'Invalid activity inventory' }

export function activityServerNow(agent: Agent, monotonicNow = performance.now()): number {
  const work = agent.activity?.work
  const received = work ? receipts.get(work) : undefined
  const server = Date.parse(work?.serverTime ?? '')
  return received !== undefined && Number.isFinite(server) ? server + Math.max(0, monotonicNow - received) : Date.now()
}
