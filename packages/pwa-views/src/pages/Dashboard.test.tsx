import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import type { Agent } from '../lib/types'

vi.mock('../hooks/useAPI', () => ({
  useAgents: vi.fn(),
  // ContextPressureList (rendered inside Dashboard) calls this at render.
  useRestartAgentSession: vi.fn(() => ({ mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false })),
}))
vi.mock('../components/TerminalPeek', () => ({ TerminalPeek: () => <div data-testid="peek" /> }))

import * as useAPIModule from '../hooks/useAPI'
import { Dashboard } from './Dashboard'

const a = (id: string): Agent => ({
  id, phase: 'Running', machine: 'm', runtime: 'claude-code', model: 'claude-sonnet-4-6',
  resources: { cpu: '1', memory: '1Gi', disk: '10Gi' },
  status: {} as Agent['status'], createdAt: '2026-07-06T10:00:00Z',
})

function mockAgents(ret: unknown) {
  vi.mocked(useAPIModule.useAgents).mockReturnValue(ret as ReturnType<typeof useAPIModule.useAgents>)
}

describe('Dashboard', () => {
  beforeEach(() => vi.clearAllMocks())

  it('renders the widgets when data is present', () => {
    mockAgents({ data: [a('carol')], isLoading: false, error: null })
    render(<MemoryRouter><Dashboard /></MemoryRouter>)
    expect(screen.getByRole('heading', { name: 'Dashboard' })).toBeInTheDocument()
    expect(screen.getByText('Agent status')).toBeInTheDocument()
    expect(screen.getByText('Recently active')).toBeInTheDocument()
    expect(screen.getByText('Context pressure')).toBeInTheDocument()
    expect(screen.getByTestId('peek')).toBeInTheDocument()
  })

  it('renders an error banner on failure', () => {
    mockAgents({ data: undefined, isLoading: false, error: new Error('boom') })
    render(<MemoryRouter><Dashboard /></MemoryRouter>)
    expect(screen.getByText(/Failed to load agents: boom/)).toBeInTheDocument()
  })
})
