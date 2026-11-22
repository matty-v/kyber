import { replaceEqualDeep } from '@tanstack/react-query'
import { describe, it, expect } from 'vitest'
import type { Agent, AgentWorkView } from './types'
import { activityGroups, activityServerNow, projectWork, recordActivityReceipt } from './activity'
const server = Date.parse('2026-11-22T20:00:00Z')
function agent(id: string, patch: Partial<AgentWorkView> = {}): Agent {
  const a = { id, phase: 'Running', activity: { work: { state: 'working', serverTime: new Date(server).toISOString(), expiresAt: new Date(server + 60000).toISOString(), observedAt: new Date(server).toISOString(), stateSince: new Date(server - 1000).toISOString(), foregroundCount: 1, backgroundCount: 0, ...patch } } } as Agent
  recordActivityReceipt([a], 100)
  return a
}
describe('current-work projection', () => {
  it('excludes stopped and pending-stop agents before considering cached work', () => {
    expect(projectWork({ ...agent('stopped'), phase: 'Stopped' }, 100).state).toBe('excluded')
    expect(projectWork({ ...agent('stopping'), desiredPhase: 'Stopped' }, 100).state).toBe('excluded')
  })
  it('never uses legacy working or heartbeats as current work', () => {
    expect(projectWork({ id: 'legacy', phase: 'Running', activity: { state: 'working', lastHeartbeatAt: new Date().toISOString() } } as Agent, 100)).toMatchObject({ state: 'unavailable', reason: 'legacy_reporting' })
  })
  it('expires using a monotonic clock and server baseline, not browser wall time', () => {
    const a = agent('a')
    expect(projectWork(a, 60099).state).toBe('working')
    expect(projectWork(a, 60100).state).toBe('unavailable')
    expect(activityServerNow(a, 5100)).toBe(server + 5000)
  })
  it('fails closed for unanchored or invalid observations', () => {
    const a = agent('a'); a.activity!.work = { ...a.activity!.work! }
    expect(projectWork(a, 100).reason).toBe('observation_unanchored')
    expect(projectWork(agent('invalid', { backgroundCount: -1 }), 100).state).toBe('unavailable')
    expect(projectWork(agent('missing time', { observedAt: undefined }), 100).state).toBe('unavailable')
    expect(projectWork(agent('future time', { observedAt: new Date(server + 1).toISOString() }), 100).state).toBe('unavailable')
    expect(projectWork(agent('overlong lease', { expiresAt: new Date(server + 60001).toISOString() }), 100).state).toBe('unavailable')
    expect(projectWork(agent('inconsistent', { state: 'background', backgroundCount: 2 }), 100).state).toBe('unavailable')
  })
  it('assigns each agent once and sorts independently of lease renewals', () => {
    const a = agent('a', { backgroundCount: 2 })
    const b = agent('b', { state: 'background', foregroundCount: 0, backgroundCount: 1 })
    const c = agent('c', { state: 'waiting', foregroundCount: 0 })
    const groups = activityGroups([c, b, a], 100)
    expect(groups.working.map(a => a.id)).toEqual(['a'])
    expect(groups.background.map(a => a.id)).toEqual(['b'])
    expect(groups.waiting.map(a => a.id)).toEqual(['c'])
    expect(Object.values(groups).flat()).toHaveLength(3)
  })
  it('anchors the structurally shared query result and never rearms an identical cached view', () => {
    const original = agent('a')
    const raw = { ...agent('a'), activity: { work: { ...original.activity!.work!, serverTime: new Date(server + 1000).toISOString() } } }
    const shared = replaceEqualDeep(original, raw)
    recordActivityReceipt([shared], 1100)
    expect(projectWork(shared, 60099).state).toBe('working')
    recordActivityReceipt([shared], 50000)
    expect(projectWork(shared, 60100).state).toBe('unavailable')
  })

})
