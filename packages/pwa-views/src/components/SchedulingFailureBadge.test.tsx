import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { SchedulingFailureBadge } from './SchedulingFailureBadge'
import type { Agent, AgentSchedulingCategory } from '../lib/types'

function agentWith(category?: AgentSchedulingCategory, lastError?: string): Agent {
  return {
    id: 'alice',
    machine: 'razer',
    scheduling: category
      ? { category, lastError, firstObservedAt: '2026-05-02T17:50:00Z' }
      : undefined,
  } as unknown as Agent
}

describe('SchedulingFailureBadge', () => {
  it('renders nothing when agent has no scheduling failure', () => {
    const { container } = render(<SchedulingFailureBadge agent={agentWith()} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders the category text', () => {
    render(<SchedulingFailureBadge agent={agentWith('Capacity', 'Insufficient memory')} />)
    expect(screen.getByTestId('scheduling-failure-badge')).toHaveTextContent('Capacity')
  })

  it('tooltip combines category + verbatim message', () => {
    render(<SchedulingFailureBadge agent={agentWith('Capacity', 'Insufficient memory')} />)
    expect(screen.getByTestId('scheduling-failure-badge')).toHaveAttribute(
      'title',
      'Capacity: Insufficient memory',
    )
  })

  it('tooltip is category-only when lastError is missing', () => {
    render(<SchedulingFailureBadge agent={agentWith('Image')} />)
    expect(screen.getByTestId('scheduling-failure-badge')).toHaveAttribute(
      'title',
      'Scheduling failure: Image',
    )
  })

  it('exposes data-category for downstream styling/tests', () => {
    render(<SchedulingFailureBadge agent={agentWith('Storage', 'PVC binding failed')} />)
    expect(screen.getByTestId('scheduling-failure-badge')).toHaveAttribute(
      'data-category',
      'Storage',
    )
  })

  it('aria-label names the failure', () => {
    render(<SchedulingFailureBadge agent={agentWith('Placement', '')} />)
    expect(screen.getByLabelText('Scheduling failure: Placement')).toBeInTheDocument()
  })
})
