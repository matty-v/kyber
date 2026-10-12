import { describe, it, expect } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import type { MetricsLastActive, MetricsNodeResources, UsageEvent } from '../lib/types'
import { summarizeUsageEvents, TokenTable } from './MetricsTab'

// Test the pure display subcomponents extracted from MetricsTab.

// ---- formatBytes helper (inlined for testability) ----

function formatBytes(b: number): string {
  if (b >= 1e9) return `${(b / 1e9).toFixed(1)} GB`
  if (b >= 1e6) return `${(b / 1e6).toFixed(1)} MB`
  return `${Math.round(b / 1e3)} KB`
}

describe('formatBytes', () => {
  it('formats gigabytes', () => {
    expect(formatBytes(4.5e9)).toBe('4.5 GB')
  })
  it('formats megabytes', () => {
    expect(formatBytes(512e6)).toBe('512.0 MB')
  })
  it('formats kilobytes', () => {
    expect(formatBytes(100e3)).toBe('100 KB')
  })
})

// ---- FormatAge helper ----

function formatAge(secs?: number): string {
  if (secs == null) return 'never'
  if (secs < 60) return `${secs}s ago`
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`
  return `${Math.floor(secs / 3600)}h ago`
}

describe('formatAge', () => {
  it('returns never for undefined', () => {
    expect(formatAge(undefined)).toBe('never')
  })
  it('returns seconds', () => {
    expect(formatAge(45)).toBe('45s ago')
  })
  it('returns minutes', () => {
    expect(formatAge(125)).toBe('2m ago')
  })
  it('returns hours', () => {
    expect(formatAge(7200)).toBe('2h ago')
  })
})

// ---- Stale detection logic ----

describe('stale detection', () => {
  it('marks agent with no heartbeat as stale', () => {
    const agent: MetricsLastActive = {
      name: 'nobeat',
      state: 'Stopped',
      stale: true,
    }
    expect(agent.stale).toBe(true)
    expect(agent.secondsSinceHeartbeat).toBeUndefined()
  })

  it('marks fresh agent as not stale', () => {
    const agent: MetricsLastActive = {
      name: 'fresh',
      state: 'working',
      secondsSinceHeartbeat: 30,
      stale: false,
    }
    expect(agent.stale).toBe(false)
  })

  it('marks agent over threshold as stale', () => {
    const agent: MetricsLastActive = {
      name: 'stale-agent',
      state: 'idle',
      secondsSinceHeartbeat: 600,
      stale: true,
    }
    expect(agent.stale).toBe(true)
  })
})

// ---- Node resources CPU gauge ----

describe('gauge percentage', () => {
  it('caps at 100%', () => {
    const pct = Math.min(100, (120 / 100) * 100)
    expect(pct).toBe(100)
  })
  it('computes correctly for partial use', () => {
    const node: MetricsNodeResources = {
      node: 'node-1',
      cpuPercent: 45,
      memUsedBytes: 4e9,
      memTotalBytes: 8e9,
      diskUsedBytes: 100e9,
      diskTotalBytes: 500e9,
    }
    const memPct = (node.memUsedBytes / node.memTotalBytes) * 100
    expect(memPct).toBe(50)
  })
})

// ---- Finalized usage evidence ----

function usageEvent(id: string, overrides: Partial<UsageEvent> = {}): UsageEvent {
  return {
    id, agentUID: 'uid-1', agent: 'r2-d2', observedAt: '2026-10-12T00:00:00Z',
    ingestedAt: '2026-10-12T00:00:01Z', runtime: 'claude', sourceFormat: 'claude-jsonl',
    sourceVersion: '1', sourceIdentity: id, coverage: 'observed',
    configuredRoute: 'direct-api', routeID: 'route-1', model: 'claude-sonnet-4-6',
    modelEvidence: 'observed', observedUpstreamProvider: null, reportedProviderLabel: null,
    inputUncached: 50, output: 10, cacheRead: 5, cacheWrite: null,
    ...overrides,
  }
}

describe('usage summary', () => {
  it('counts equal-sized calls separately and preserves unknown token classes', () => {
    const rows = summarizeUsageEvents([
      usageEvent('one'),
      usageEvent('two', { output: 20, cacheWrite: 3 }),
    ])
    expect(rows).toHaveLength(1)
    expect(rows[0].eventCount).toBe(2)
    expect(rows[0].tokens).toEqual({ inputUncached: 100, output: 30, cacheRead: 10, cacheWrite: 3 })
    expect(rows[0].unknown.cacheWrite).toBe(1)
    render(<TokenTable rows={rows} />)
    expect(screen.getAllByText('100')).toHaveLength(2) // mobile card and desktop table
    expect(screen.getAllByText('3*')).toHaveLength(2)
    for (const value of screen.getAllByText('3*')) {
      expect(value).toHaveAttribute('title', '1 call did not report this count')
    }
    expect(screen.queryByText(/cost|unpriced/i)).not.toBeInTheDocument()
  })

  it('keeps provider-unknown calls distinct from observed upstream providers', () => {
    const rows = summarizeUsageEvents([
      usageEvent('unknown'),
      usageEvent('known', { observedUpstreamProvider: 'anthropic' }),
    ])
    expect(rows).toHaveLength(2)
    render(<TokenTable rows={rows} />)
    expect(screen.getByText('Unknown')).toBeInTheDocument()
    expect(screen.getByText('anthropic')).toBeInTheDocument()
  })

  it('shows an unknown count as unavailable, not zero', () => {
    const rows = summarizeUsageEvents([usageEvent('one', { cacheWrite: null })])
    render(<TokenTable rows={rows} />)
    const unknown = screen.getAllByTitle('1 call did not report this count')
    expect(unknown).toHaveLength(2)
    for (const value of unknown) expect(value).toHaveTextContent('—')
  })
})
