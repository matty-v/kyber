import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { TokenUsageCard } from './TokenUsage'
import type { TokenUsage } from '../lib/types'

function usage(overrides: Partial<TokenUsage> = {}): TokenUsage {
  return {
    model: 'claude-opus-4-7',
    tokens: { used: 300_000, limit: 1_000_000, input: 0, cacheCreation: 0, cacheRead: 0 },
    percentage: 30,
    effortLevel: 'high',
    speed: 'standard',
    updatedAt: '2026-06-01T00:00:00Z',
    contextWindowKnown: true,
    ...overrides,
  }
}

describe('TokenUsageCard', () => {
  it('shows a precise percentage when the context window is known', () => {
    render(<TokenUsageCard data={usage({ contextWindowKnown: true, percentage: 30 })} isLoading={false} />)
    expect(screen.getByText(/30\.0%/)).toBeInTheDocument()
    // No estimate marker when the window is known.
    expect(screen.queryByText(/≈/)).toBeNull()
    expect(screen.queryByText(/unverified window/i)).toBeNull()
  })

  it('flags the percentage as an estimate when the context window is unknown (#396)', () => {
    render(
      <TokenUsageCard
        data={usage({ contextWindowKnown: false, percentage: 30, tokens: { used: 60_000, limit: 200_000, input: 0, cacheCreation: 0, cacheRead: 0 } })}
        isLoading={false}
      />,
    )
    // The percentage is marked as an estimate, not shown as a confident number.
    expect(screen.getByText(/≈/)).toBeInTheDocument()
    expect(screen.getByText(/unverified window/i)).toBeInTheDocument()
  })

  it('does not show "over budget" for an unknown-window estimate even if pct > 100', () => {
    render(
      <TokenUsageCard
        data={usage({ contextWindowKnown: false, percentage: 150 })}
        isLoading={false}
      />,
    )
    expect(screen.queryByText(/over budget/i)).toBeNull()
    expect(screen.getByText(/≈/)).toBeInTheDocument()
  })

  it('still shows "over budget" for a known window over 100%', () => {
    render(
      <TokenUsageCard data={usage({ contextWindowKnown: true, percentage: 120 })} isLoading={false} />,
    )
    expect(screen.getByText(/over budget/i)).toBeInTheDocument()
  })
})
