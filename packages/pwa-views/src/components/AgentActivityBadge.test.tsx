import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { AgentActivityBadge } from './AgentActivityBadge'
import type { Agent, AgentActivityState } from '../lib/types'

function agentWith(state?: AgentActivityState, lastActivityAt?: string): Agent {
  return {
    id: 'alice',
    activity: state ? { state, lastActivityAt } : undefined,
  } as unknown as Agent
}

describe('AgentActivityBadge', () => {
  it('renders nothing when no activity status', () => {
    const { container } = render(<AgentActivityBadge agent={agentWith()} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders nothing when state is unknown', () => {
    const { container } = render(<AgentActivityBadge agent={agentWith('unknown')} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders "Working" badge with success styling when working', () => {
    render(<AgentActivityBadge agent={agentWith('working')} />)
    const badge = screen.getByTestId('agent-activity-badge')
    expect(badge).toHaveAttribute('data-state', 'working')
    expect(badge).toHaveTextContent('Working')
    expect(badge.className).toContain('text-success')
  })

  it('renders "Idle <relative>" when idle', () => {
    const fourMinAgo = new Date(Date.now() - 4 * 60_000).toISOString()
    render(<AgentActivityBadge agent={agentWith('idle', fourMinAgo)} />)
    const badge = screen.getByTestId('agent-activity-badge')
    expect(badge.textContent).toMatch(/^Idle 4m ago$/)
  })

  it('renders "Idle" alone when lastActivityAt missing', () => {
    render(<AgentActivityBadge agent={agentWith('idle')} />)
    expect(screen.getByTestId('agent-activity-badge')).toHaveTextContent('Idle')
  })
})
