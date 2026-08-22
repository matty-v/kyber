import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import type { Agent } from '../lib/types'
import { AgentStatusBar } from './AgentStatusBar'

const a = (id: string, phase: Agent['phase']): Agent => ({
  id, phase, machine: 'm', runtime: 'claude-code', model: 'claude-sonnet-4-6',
  resources: { cpu: '1', memory: '1Gi', disk: '10Gi' },
  status: {} as Agent['status'], createdAt: '2026-07-06T10:00:00Z',
})

describe('AgentStatusBar', () => {
  it('shows total and a legend entry with count per phase', () => {
    render(<AgentStatusBar agents={[a('x', 'Running'), a('y', 'Running'), a('z', 'NeedsAuth')]} />)
    expect(screen.getByText('3 total')).toBeInTheDocument()
    expect(screen.getByText('NeedsAuth')).toBeInTheDocument()
  })
  it('renders an empty state with no agents', () => {
    render(<AgentStatusBar agents={[]} />)
    expect(screen.getByText('No agents')).toBeInTheDocument()
  })
})
