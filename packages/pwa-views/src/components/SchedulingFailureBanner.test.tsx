import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { SchedulingFailureBanner, formatObservedAgo } from './SchedulingFailureBanner'
import type { Agent, AgentSchedulingCategory } from '../lib/types'

function agentWith(category?: AgentSchedulingCategory, overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'alice',
    machine: 'razer',
    scheduling: category
      ? {
          category,
          lastError: 'sample scheduler message',
          firstObservedAt: new Date(Date.now() - 5 * 60_000).toISOString(), // 5 min ago
        }
      : undefined,
    ...overrides,
  } as unknown as Agent
}

describe('SchedulingFailureBanner', () => {
  it('renders nothing when agent has no scheduling failure', () => {
    const { container } = render(<SchedulingFailureBanner agent={agentWith()} />)
    expect(container.firstChild).toBeNull()
  })

  it('explains automatic recovery while waiting for machine capacity', () => {
    render(<SchedulingFailureBanner agent={agentWith(undefined, {
      phase: 'WaitingForMachine',
      status: { phase: 'WaitingForMachine', message: 'waiting detail' },
    })} />)
    expect(screen.getByTestId('machine-wait-banner')).toHaveTextContent('Waiting for machine capacity')
    expect(screen.getByText(/restart this agent automatically/)).toBeInTheDocument()
    expect(screen.getByText('Technical details')).toBeInTheDocument()
  })

  it('renders Capacity headline + remediation that names the machine', () => {
    render(<SchedulingFailureBanner agent={agentWith('Capacity')} />)
    expect(screen.getByTestId('scheduling-failure-banner')).toHaveAttribute(
      'data-category',
      'Capacity',
    )
    expect(screen.getByText(/Pod can't schedule — capacity full/)).toBeInTheDocument()
    expect(screen.getByText(/razer doesn't have enough capacity/)).toBeInTheDocument()
  })

  it('renders Placement copy', () => {
    render(<SchedulingFailureBanner agent={agentWith('Placement')} />)
    expect(screen.getByText(/no matching node/)).toBeInTheDocument()
    expect(screen.getByText(/affinity \/ taint requirements/)).toBeInTheDocument()
  })

  it('renders Image copy', () => {
    render(<SchedulingFailureBanner agent={agentWith('Image')} />)
    expect(screen.getByText(/Container image pull failed/)).toBeInTheDocument()
    expect(screen.getByText(/registry/)).toBeInTheDocument()
  })

  it('renders Storage copy', () => {
    render(<SchedulingFailureBanner agent={agentWith('Storage')} />)
    expect(screen.getByText(/Volume binding failed/)).toBeInTheDocument()
  })

  it('renders Other with no remediation paragraph (verbatim only)', () => {
    render(<SchedulingFailureBanner agent={agentWith('Other')} />)
    expect(screen.getByText(/Pod scheduling failure/)).toBeInTheDocument()
    expect(screen.queryByText(/Reduce the request/)).not.toBeInTheDocument()
    expect(screen.queryByText(/affinity/)).not.toBeInTheDocument()
    // Verbatim message still renders
    expect(screen.getByTestId('scheduling-failure-message')).toHaveTextContent(
      'sample scheduler message',
    )
  })

  it('keeps the verbatim scheduler message in collapsed technical details', () => {
    render(<SchedulingFailureBanner agent={agentWith('Capacity')} />)
    const pre = screen.getByTestId('scheduling-failure-message')
    expect(pre).toHaveTextContent('sample scheduler message')
    expect(pre.tagName).toBe('PRE')
  })

  it('renders the firstObservedAt timestamp + category in the footer', () => {
    render(<SchedulingFailureBanner agent={agentWith('Capacity')} />)
    expect(screen.getByText(/First observed 5 min ago/)).toBeInTheDocument()
    expect(screen.getByText(/category: Capacity/)).toBeInTheDocument()
  })

  it('omits the "First observed" prefix when firstObservedAt is missing', () => {
    const agent = agentWith('Capacity')
    if (agent.scheduling) agent.scheduling.firstObservedAt = undefined
    render(<SchedulingFailureBanner agent={agent} />)
    expect(screen.queryByText(/First observed/)).not.toBeInTheDocument()
    expect(screen.getByText(/category: Capacity/)).toBeInTheDocument()
  })

  it('falls back to Other copy when category is unknown (forward-compat)', () => {
    const agent = agentWith('Capacity')
    if (agent.scheduling) {
      agent.scheduling.category = 'NewCategory' as AgentSchedulingCategory
    }
    render(<SchedulingFailureBanner agent={agent} />)
    expect(screen.getByText(/Pod scheduling failure/)).toBeInTheDocument()
  })
})

describe('formatObservedAgo', () => {
  const NOW = new Date('2026-05-02T18:00:00Z')

  it('"just now" under 30s', () => {
    expect(formatObservedAgo(new Date(NOW.getTime() - 5_000).toISOString(), NOW)).toBe('just now')
  })

  it('seconds for 30-59s', () => {
    expect(formatObservedAgo(new Date(NOW.getTime() - 45_000).toISOString(), NOW)).toBe('45 sec ago')
  })

  it('minutes for 1m-59m', () => {
    expect(formatObservedAgo(new Date(NOW.getTime() - 5 * 60_000).toISOString(), NOW)).toBe('5 min ago')
  })

  it('hours for 1h-23h', () => {
    expect(formatObservedAgo(new Date(NOW.getTime() - 3 * 3600_000).toISOString(), NOW)).toBe('3 h ago')
  })

  it('days past 24h', () => {
    expect(formatObservedAgo(new Date(NOW.getTime() - 2 * 86400_000).toISOString(), NOW)).toBe('2 d ago')
  })

  it('"recently" for unparseable timestamp', () => {
    expect(formatObservedAgo('not-a-date', NOW)).toBe('recently')
  })
})
