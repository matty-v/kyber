import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { AgentActivityDot, formatRelativeTime } from './AgentActivityDot'
import type { Agent, AgentActivityState } from '../lib/types'

function agentWith(state?: AgentActivityState, lastActivityAt?: string): Agent {
  return {
    id: 'alice',
    activity: state
      ? { state, lastActivityAt, lastHeartbeatAt: '2026-05-03T22:00:00Z' }
      : undefined,
  } as unknown as Agent
}

describe('AgentActivityDot', () => {
  it('renders nothing when agent has no activity status', () => {
    const { container } = render(<AgentActivityDot agent={agentWith()} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders nothing when state is unknown', () => {
    const { container } = render(<AgentActivityDot agent={agentWith('unknown')} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders nothing when state is empty string', () => {
    const { container } = render(<AgentActivityDot agent={agentWith('')} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders pulsing green dot when working', () => {
    render(<AgentActivityDot agent={agentWith('working')} />)
    const dot = screen.getByTestId('agent-activity-dot')
    expect(dot).toHaveAttribute('data-state', 'working')
    expect(dot.className).toContain('bg-success')
    expect(dot.className).toContain('motion-safe:animate-pulse')
  })

  it('renders gray static dot when idle', () => {
    render(<AgentActivityDot agent={agentWith('idle', '2026-05-03T22:00:00Z')} />)
    const dot = screen.getByTestId('agent-activity-dot')
    expect(dot).toHaveAttribute('data-state', 'idle')
    expect(dot.className).toContain('bg-text-disabled')
    expect(dot.className).not.toContain('animate-pulse')
  })

  it('tooltip says "Working" for working state', () => {
    render(<AgentActivityDot agent={agentWith('working')} />)
    expect(screen.getByTestId('agent-activity-dot')).toHaveAttribute('title', 'Working')
  })

  it('tooltip says "Idle <relative>" for idle state with timestamp', () => {
    const tenMinAgo = new Date(Date.now() - 10 * 60_000).toISOString()
    render(<AgentActivityDot agent={agentWith('idle', tenMinAgo)} />)
    expect(screen.getByTestId('agent-activity-dot').getAttribute('title')).toMatch(/^Idle 10m ago$/)
  })

  it('tooltip is just "Idle" when lastActivityAt is missing', () => {
    render(<AgentActivityDot agent={agentWith('idle')} />)
    expect(screen.getByTestId('agent-activity-dot')).toHaveAttribute('title', 'Idle')
  })
})

describe('formatRelativeTime', () => {
  const NOW = new Date('2026-05-03T22:00:00Z')

  it('seconds for <60s', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 5_000).toISOString(), NOW)).toBe('5s ago')
  })
  it('minutes for 1m-59m', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 8 * 60_000).toISOString(), NOW)).toBe('8m ago')
  })
  it('hours for 1h-23h', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 4 * 3600_000).toISOString(), NOW)).toBe('4h ago')
  })
  it('days past 24h', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 3 * 86400_000).toISOString(), NOW)).toBe('3d ago')
  })
  it('"recently" for unparseable timestamp', () => {
    expect(formatRelativeTime('garbage', NOW)).toBe('recently')
  })
})
