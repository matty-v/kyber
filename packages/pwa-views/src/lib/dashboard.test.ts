import { describe, it, expect } from 'vitest'
import type { Agent } from './types'
import {
  phaseCounts, isAttentionPhase, isStale, rankByActivity,
  rankByContext, contextTone, formatAgo, formatTokens, STALE_THRESHOLD_MS,
} from './dashboard'

const NOW = Date.parse('2026-07-06T12:00:00Z')

function agent(over: Partial<Agent> & { id: string }): Agent {
  return {
    id: over.id, phase: 'Running', machine: 'm', runtime: 'claude-code',
    model: 'claude-sonnet-4-6', scaling: 'warm',
    resources: { cpu: '1', memory: '1Gi', disk: '10Gi' },
    status: {} as Agent['status'], createdAt: '2026-07-06T10:00:00Z', ...over,
  }
}

describe('phaseCounts', () => {
  it('counts by phase in canonical order, healthy before attention', () => {
    const out = phaseCounts([
      agent({ id: 'a', phase: 'Failed' }), agent({ id: 'b', phase: 'Running' }),
      agent({ id: 'c', phase: 'Running' }), agent({ id: 'd', phase: 'NeedsAuth' }),
    ])
    expect(out).toEqual([
      { phase: 'Running', count: 2 },
      { phase: 'NeedsAuth', count: 1 },
      { phase: 'Failed', count: 1 },
    ])
  })
  it('appends unknown phases alphabetically after known', () => {
    const out = phaseCounts([agent({ id: 'a', phase: 'Zeta' as Agent['phase'] }), agent({ id: 'b', phase: 'Running' })])
    expect(out.map((p) => p.phase)).toEqual(['Running', 'Zeta'])
  })
})

describe('isAttentionPhase', () => {
  it('flags NeedsAuth/MemoryExhausted/Failed only', () => {
    expect(isAttentionPhase('NeedsAuth')).toBe(true)
    expect(isAttentionPhase('MemoryExhausted')).toBe(true)
    expect(isAttentionPhase('Failed')).toBe(true)
    expect(isAttentionPhase('Running')).toBe(false)
  })
})

describe('isStale', () => {
  it('true when last heartbeat older than threshold', () => {
    const old = new Date(NOW - STALE_THRESHOLD_MS - 1000).toISOString()
    expect(isStale(agent({ id: 'a', activity: { lastHeartbeatAt: old } }), NOW)).toBe(true)
  })
  it('false when fresh or missing', () => {
    const fresh = new Date(NOW - 1000).toISOString()
    expect(isStale(agent({ id: 'a', activity: { lastHeartbeatAt: fresh } }), NOW)).toBe(false)
    expect(isStale(agent({ id: 'b' }), NOW)).toBe(false)
  })
})

describe('rankByActivity', () => {
  it('sorts by most-recent activity desc, drops agents with no activity, respects limit', () => {
    const mk = (id: string, secsAgo: number) =>
      agent({ id, activity: { lastActivityAt: new Date(NOW - secsAgo * 1000).toISOString() } })
    const out = rankByActivity([mk('old', 300), agent({ id: 'none' }), mk('new', 10), mk('mid', 60)], 2)
    expect(out.map((a) => a.id)).toEqual(['new', 'mid'])
  })
  it('falls back to lastHeartbeatAt when lastActivityAt absent', () => {
    const out = rankByActivity([agent({ id: 'h', activity: { lastHeartbeatAt: new Date(NOW - 5000).toISOString() } })], 5)
    expect(out.map((a) => a.id)).toEqual(['h'])
  })
})

describe('rankByContext', () => {
  const withCtx = (id: string, pct: number, known = true) =>
    agent({ id, tokenUsage: { model: 'claude-sonnet-4-6', tokens: { used: pct * 2000, limit: 200000, input: 0, cacheCreation: 0, cacheRead: 0 }, percentage: pct, effortLevel: '', speed: '', updatedAt: '', contextWindowKnown: known } })
  it('sorts by percentage desc, excludes snapshot-less and unknown-window agents, respects limit', () => {
    const out = rankByContext([withCtx('lo', 20), agent({ id: 'nosnap' }), withCtx('hi', 90), withCtx('unk', 99, false), withCtx('mid', 55)], 2)
    expect(out.map((r) => r.agent.id)).toEqual(['hi', 'mid'])
  })
})

describe('contextTone', () => {
  it('green <60, amber 60–85, red >85', () => {
    expect(contextTone(59)).toBe('success')
    expect(contextTone(60)).toBe('warn')
    expect(contextTone(85)).toBe('warn')
    expect(contextTone(86)).toBe('danger')
  })
})

describe('formatAgo', () => {
  it('formats seconds/minutes/hours, em dash for missing', () => {
    expect(formatAgo(new Date(NOW - 30_000).toISOString(), NOW)).toBe('30s ago')
    expect(formatAgo(new Date(NOW - 120_000).toISOString(), NOW)).toBe('2m ago')
    expect(formatAgo(new Date(NOW - 3_600_000).toISOString(), NOW)).toBe('1h ago')
    expect(formatAgo(undefined, NOW)).toBe('—')
  })
})

describe('formatTokens', () => {
  it('compacts to k / M', () => {
    expect(formatTokens(164_000)).toBe('164k')
    expect(formatTokens(1_000_000)).toBe('1M')
    expect(formatTokens(950)).toBe('950')
  })
})
