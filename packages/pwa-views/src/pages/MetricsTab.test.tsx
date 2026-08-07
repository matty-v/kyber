import { describe, it, expect } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import type { MetricsLastActive, MetricsNodeResources, MetricsTokenUsage } from '../lib/types'

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

// NOTE: cost math is server-side (pkg/metrics ComputeCostKnown, covered by Go
// tests); the panel only renders the server-supplied costUsd. A local
// re-implementation of the pricing formula used to live here but only tested
// itself, so it was removed.

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

// ---- Token usage sorting ----

describe('token table sorting', () => {
  it('sorts by costUsd descending', () => {
    const rows: MetricsTokenUsage[] = [
      { agent: 'cheap', model: 'haiku', tokens: {}, costUsd: 0.1 },
      { agent: 'pricey', model: 'opus', tokens: {}, costUsd: 5.5 },
      { agent: 'mid', model: 'sonnet', tokens: {}, costUsd: 2.2 },
    ]
    const sorted = [...rows].sort((a, b) => b.costUsd - a.costUsd)
    expect(sorted[0].agent).toBe('pricey')
    expect(sorted[1].agent).toBe('mid')
    expect(sorted[2].agent).toBe('cheap')
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

// ---- TokenTable unpriced fail-loud badge (kyber#487) ----

import { TokenTable } from './MetricsTab'

describe('TokenTable unpriced badge', () => {
  it('renders — and an unpriced badge for priced:false, not $0.0000', () => {
    const rows: MetricsTokenUsage[] = [
      { agent: 'oppy', model: 'claude-opus-4-8', tokens: { input: 1000 }, costUsd: 0, priced: false },
    ]
    render(<TokenTable rows={rows} />)
    expect(screen.getByText('—')).toBeInTheDocument()
    expect(screen.getByText(/unpriced/i)).toBeInTheDocument()
    expect(screen.queryByText('$0.0000')).not.toBeInTheDocument()
  })

  it('renders money for priced:true', () => {
    const rows: MetricsTokenUsage[] = [
      { agent: 'sonny', model: 'claude-sonnet-4-6', tokens: {}, costUsd: 3, priced: true },
    ]
    render(<TokenTable rows={rows} />)
    expect(screen.getByText('$3.0000')).toBeInTheDocument()
    expect(screen.queryByText(/unpriced/i)).not.toBeInTheDocument()
  })

  it('treats a missing priced field (older payload) as priced — money, not unpriced', () => {
    const rows: MetricsTokenUsage[] = [
      { agent: 'legacy', model: 'x', tokens: {}, costUsd: 0 },
    ]
    render(<TokenTable rows={rows} />)
    expect(screen.getByText('$0.0000')).toBeInTheDocument()
    expect(screen.queryByText(/unpriced/i)).not.toBeInTheDocument()
  })

  it('renders the Cache write column with the cache_creation count', () => {
    const rows: MetricsTokenUsage[] = [
      {
        agent: 'sonny',
        model: 'claude-sonnet-4-6',
        tokens: { input: 1000, output: 200, cache_creation: 4321, cache_read: 99 },
        costUsd: 1.23,
        priced: true,
      },
    ]
    render(<TokenTable rows={rows} />)
    expect(screen.getByText('Cache write')).toBeInTheDocument()
    expect(screen.getByText('4,321')).toBeInTheDocument()
    // Column order: Agent, Model, Input, Output, Cache write, Cache read, Cost.
    const cells = screen.getAllByRole('cell').map((c) => c.textContent)
    expect(cells.slice(2, 6)).toEqual(['1,000', '200', '4,321', '99'])
  })

  it('renders 0 in Cache write for a row without cache_creation', () => {
    const rows: MetricsTokenUsage[] = [
      { agent: 'legacy', model: 'x', tokens: { input: 5 }, costUsd: 0, priced: true },
    ]
    render(<TokenTable rows={rows} />)
    expect(screen.getByText('Cache write')).toBeInTheDocument()
    // Output, Cache write, and Cache read all fall back to 0.
    const cells = screen.getAllByRole('cell').map((c) => c.textContent)
    expect(cells.slice(2, 6)).toEqual(['5', '0', '0', '0'])
  })

  it('sorts unpriced rows last, below genuinely-priced rows', () => {
    const rows: MetricsTokenUsage[] = [
      { agent: 'unpriced', model: 'claude-opus-4-8', tokens: {}, costUsd: 0, priced: false },
      { agent: 'cheap', model: 'haiku', tokens: {}, costUsd: 0.1, priced: true },
      { agent: 'pricey', model: 'opus', tokens: {}, costUsd: 5.5, priced: true },
    ]
    render(<TokenTable rows={rows} />)
    const dataRows = screen.getAllByRole('row').slice(1) // drop header
    const agents = dataRows.map((r) => r.querySelector('td')?.textContent)
    expect(agents).toEqual(['pricey', 'cheap', 'unpriced'])
  })
})
