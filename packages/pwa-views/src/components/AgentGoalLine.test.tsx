import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { AgentGoalLine } from './AgentGoalLine'

describe('AgentGoalLine', () => {
  it('renders no spacer when no goal exists', () => {
    const { container } = render(<AgentGoalLine />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders goal text as plain text with a full-value title', () => {
    render(<AgentGoalLine goal={{
      summary: '<b>Review MAT-62</b>', source: 'agent',
      acceptedAt: '2026-09-16T12:00:00Z', updatedAt: '2026-09-16T12:00:01Z',
    }} />)
    const goal = screen.getByTestId('agent-goal')
    expect(goal).toHaveTextContent('<b>Review MAT-62</b>')
    expect(goal).toHaveAttribute('title', '<b>Review MAT-62</b>')
    expect(goal).toHaveClass('min-w-0', 'max-w-full', 'truncate')
    expect(goal.querySelector('b')).toBeNull()
  })

  it('visually de-emphasizes the privacy-safe platform fallback', () => {
    render(<AgentGoalLine goal={{
      summary: 'Working on a new request', source: 'platform',
      acceptedAt: '2026-09-16T12:00:00Z', updatedAt: '2026-09-16T12:00:00Z',
    }} />)
    expect(screen.getByTestId('agent-goal')).toHaveClass('text-text-muted')
  })
})
