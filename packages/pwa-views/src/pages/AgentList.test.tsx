import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import type { Agent } from '../lib/types'
import { mockIdleAgent, mockNoActivityAgent } from '../mocks/fixtures'

// AgentList.test.tsx — kyber#417. The Agent list surfaces the same
// "Idle <relative-time>" / "Working" activity text that the detail view
// shows, by mounting AgentActivityBadge on both render paths. The page
// renders BOTH the mobile card tree (md:hidden) and the desktop table tree
// (hidden md:block) into the DOM under jsdom (CSS visibility isn't applied),
// so each agent yields TWO badge elements — assert with getAllByTestId.

// Mock the data hooks AgentList consumes. useAgents feeds the list; the
// mutation hooks only need an isPending flag + a mutateAsync stub so the
// page's `isActing` computation and action menu wire up without a backend.
vi.mock('../hooks/useAPI', () => ({
  useAgents: vi.fn(),
  useStartAgent: vi.fn(),
  useStopAgent: vi.fn(),
  useRestartAgent: vi.fn(),
  useSuspendAgent: vi.fn(),
  useDeleteAgent: vi.fn(),
}))

// Replace useNavigate with a spy; keep the real MemoryRouter/usePrefixedPath.
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom')
  return { ...actual, useNavigate: () => vi.fn() }
})

import * as useAPIModule from '../hooks/useAPI'
import { AgentList } from './AgentList'

function renderList(agents: Agent[]) {
  vi.mocked(useAPIModule.useAgents).mockReturnValue({
    data: agents,
    isLoading: false,
    error: null,
  } as unknown as ReturnType<typeof useAPIModule.useAgents>)
  const idleMutation = { mutateAsync: vi.fn().mockResolvedValue({}), isPending: false }
  for (const hook of [
    useAPIModule.useStartAgent,
    useAPIModule.useStopAgent,
    useAPIModule.useRestartAgent,
    useAPIModule.useSuspendAgent,
    useAPIModule.useDeleteAgent,
  ]) {
    vi.mocked(hook).mockReturnValue(
      idleMutation as unknown as ReturnType<typeof hook>,
    )
  }
  return render(
    <MemoryRouter>
      <AgentList />
    </MemoryRouter>,
  )
}

describe('AgentList activity badge (kyber#417)', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('renders "Idle <relative>" text on both list render paths for an idle agent', () => {
    renderList([mockIdleAgent])
    // One badge per render path (mobile card + desktop table row).
    const badges = screen.getAllByTestId('agent-activity-badge')
    expect(badges).toHaveLength(2)
    for (const badge of badges) {
      expect(badge).toHaveAttribute('data-state', 'idle')
      expect(badge).toHaveClass('whitespace-nowrap')
      expect(badge.textContent).toMatch(/^Idle 1h ago$/)
      expect(badge.querySelector('[aria-hidden="true"]')).toBeNull()
    }
  })

  it('renders no activity badge for an agent with no reported activity', () => {
    const { container } = renderList([mockNoActivityAgent])
    expect(screen.queryAllByTestId('agent-activity-badge')).toHaveLength(0)
    // The list view has no standalone activity dots.
    expect(screen.queryAllByTestId('agent-activity-dot')).toHaveLength(0)
    // And no empty spacer div is emitted in the badge's place — a no-activity
    // card must keep its exact prior layout, not gain a stray mt-1 gap that
    // nudges the model·machine line down (kyber#417 backward-compat AC).
    const emptySpacers = Array.from(container.querySelectorAll('div.mt-1')).filter(
      (d) => d.children.length === 0 && !d.textContent,
    )
    expect(emptySpacers).toHaveLength(0)
  })

  it('shows the observed model when the agent uses the harness default', () => {
    renderList([{ ...mockNoActivityAgent, model: '', currentModel: 'claude-sonnet-5' }])
    expect(screen.getByText('claude-sonnet-5')).toBeInTheDocument()
    expect(screen.getByText(/claude-sonnet-5 ·/)).toBeInTheDocument()
  })
})
