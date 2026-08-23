import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import type { Agent } from '../lib/types'
import { RecentlyActiveList } from './RecentlyActiveList'

const NOW = Date.parse('2026-07-06T12:00:00Z')
const a = (id: string, over: Partial<Agent> = {}): Agent => ({
  id, phase: 'Running', machine: 'm', runtime: 'claude-code', model: 'claude-sonnet-4-6',
  resources: { cpu: '1', memory: '1Gi', disk: '10Gi' },
  status: {} as Agent['status'], createdAt: '2026-07-06T10:00:00Z', ...over,
})

const render1 = (agents: Agent[]) =>
  render(<MemoryRouter><RecentlyActiveList agents={agents} now={NOW} /></MemoryRouter>)

describe('RecentlyActiveList', () => {
  it('links each row to the agent detail page', () => {
    render1([a('carol', { activity: { lastActivityAt: new Date(NOW - 30_000).toISOString(), state: 'working' } })])
    expect(screen.getByRole('link', { name: /carol/ })).toHaveAttribute('href', '/agents/carol')
    expect(screen.getByText('30s ago · working')).toBeInTheDocument()
  })
  it('shows an attention badge for a NeedsAuth agent', () => {
    render1([a('alice', { phase: 'NeedsAuth', activity: { lastActivityAt: new Date(NOW - 60_000).toISOString() } })])
    expect(screen.getByText('NeedsAuth')).toBeInTheDocument()
  })
  it('empty state when nobody has activity', () => {
    render1([a('quiet')])
    expect(screen.getByText('No activity yet')).toBeInTheDocument()
  })
})
