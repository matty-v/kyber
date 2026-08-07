import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import type { Agent } from '../lib/types'
import { ContextPressureList } from './ContextPressureList'
import * as useAPIModule from '../hooks/useAPI'

vi.mock('../hooks/useAPI', () => ({
  useRestartAgentSession: vi.fn(),
}))

interface MutationMock {
  mutate: ReturnType<typeof vi.fn>
  mutateAsync: ReturnType<typeof vi.fn>
  isPending: boolean
}
function newMutationMock(overrides: Partial<MutationMock> = {}): MutationMock {
  return { mutate: vi.fn(), mutateAsync: vi.fn().mockResolvedValue(undefined), isPending: false, ...overrides }
}
let restartMock: MutationMock

beforeEach(() => {
  restartMock = newMutationMock()
  vi.mocked(useAPIModule.useRestartAgentSession).mockReturnValue(
    restartMock as unknown as ReturnType<typeof useAPIModule.useRestartAgentSession>,
  )
})

const ctx = (
  id: string, pct: number, used: number, limit: number,
  model = 'claude-sonnet-4-6', phase: Agent['phase'] = 'Running',
): Agent => ({
  id, phase, machine: 'm', runtime: 'claude-code', model,
  scaling: 'warm', resources: { cpu: '1', memory: '1Gi', disk: '10Gi' },
  status: {} as Agent['status'], createdAt: '2026-07-06T10:00:00Z',
  tokenUsage: { model, tokens: { used, limit, input: 0, cacheCreation: 0, cacheRead: 0 }, percentage: pct, effortLevel: '', speed: '', updatedAt: '', contextWindowKnown: true },
})
const plain = (id: string): Agent => ({ ...ctx(id, 0, 0, 0), tokenUsage: undefined })

const render1 = (agents: Agent[]) =>
  render(<MemoryRouter><ContextPressureList agents={agents} /></MemoryRouter>)

describe('ContextPressureList', () => {
  it('orders by percentage desc and shows model + window + percent', () => {
    render1([ctx('lo', 40, 80_000, 200_000, 'claude-opus-4-8'), ctx('hi', 82, 164_000, 200_000)])
    const links = screen.getAllByRole('link')
    expect(links[0]).toHaveTextContent('hi')
    expect(screen.getByText('164k / 200k')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet-4-6')).toBeInTheDocument()
    expect(screen.getByText('82%')).toBeInTheDocument()
  })
  it('empty state when no agent has context data', () => {
    render1([plain('a')])
    expect(screen.getByText('No context data yet')).toBeInTheDocument()
  })

  it('renders a reset button for each running agent', () => {
    render1([ctx('hi', 82, 164_000, 200_000), ctx('lo', 40, 80_000, 200_000)])
    expect(screen.getByRole('button', { name: 'Reset session for hi' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Reset session for lo' })).toBeInTheDocument()
  })

  it('omits the reset button for non-running agents', () => {
    render1([ctx('starting', 30, 60_000, 200_000, 'claude-opus-4-8', 'Pending')])
    expect(screen.queryByRole('button', { name: /Reset session/ })).not.toBeInTheDocument()
  })

  it('opens the confirm dialog and restarts the session on confirm', async () => {
    render1([ctx('hi', 82, 164_000, 200_000)])
    // Dialog is not shown until the reset button is tapped.
    expect(screen.queryByText('Restart session?')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Reset session for hi' }))
    expect(screen.getByText('Restart session?')).toBeInTheDocument()
    expect(screen.getByText(/the conversation \/ context is lost/i)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(restartMock.mutateAsync).toHaveBeenCalledWith('hi'))
    // Dialog dismisses after the action resolves.
    await waitFor(() => expect(screen.queryByText('Restart session?')).not.toBeInTheDocument())
  })

  it('cancel closes the dialog without restarting', () => {
    render1([ctx('hi', 82, 164_000, 200_000)])
    fireEvent.click(screen.getByRole('button', { name: 'Reset session for hi' }))
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByText('Restart session?')).not.toBeInTheDocument()
    expect(restartMock.mutateAsync).not.toHaveBeenCalled()
  })
})
