import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import { CommandPalette } from './CommandPalette'
import * as useAPIModule from '../hooks/useAPI'
import type { Agent, Machine } from '../lib/types'

vi.mock('../hooks/useAPI', () => ({
  useAgents: vi.fn(),
  useMachines: vi.fn(),
}))

const navigateMock = vi.fn()
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom')
  return { ...actual, useNavigate: () => navigateMock }
})

function makeAgent(id: string, phase = 'Running'): Agent {
  return {
    id,
    phase: phase as Agent['phase'],
    machine: 'm1',
    runtime: 'claude-code',
    model: 'sonnet',
    resources: { cpu: '500m', memory: '1Gi' } as unknown as Agent['resources'],
    status: { phase: phase as Agent['phase'] } as unknown as Agent['status'],
    createdAt: '2026-05-08T00:00:00Z',
  }
}

function makeMachine(id: string, phase = 'Ready'): Machine {
  return {
    id,
    phase: phase as Machine['phase'],
    spec: {} as Machine['spec'],
    status: {} as Machine['status'],
    createdAt: '2026-05-08T00:00:00Z',
  }
}

function setHooks({
  agents = [] as Agent[],
  machines = [] as Machine[],
  agentsLoading = false,
  machinesLoading = false,
}: {
  agents?: Agent[]
  machines?: Machine[]
  agentsLoading?: boolean
  machinesLoading?: boolean
} = {}) {
  vi.mocked(useAPIModule.useAgents).mockReturnValue({
    data: agents,
    isLoading: agentsLoading,
    error: null,
  } as unknown as ReturnType<typeof useAPIModule.useAgents>)
  vi.mocked(useAPIModule.useMachines).mockReturnValue({
    data: machines,
    isLoading: machinesLoading,
    error: null,
  } as unknown as ReturnType<typeof useAPIModule.useMachines>)
}

function renderPalette(node: ReactNode) {
  return render(
    <MemoryRouter>
      {node}
    </MemoryRouter>,
  )
}

// Polyfill ResizeObserver for jsdom — cmdk reads it for layout sizing.
if (typeof globalThis.ResizeObserver === 'undefined') {
  ;(globalThis as unknown as { ResizeObserver: unknown }).ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
}

// Polyfill Element.prototype.scrollIntoView — cmdk auto-scrolls the active item.
if (typeof Element !== 'undefined' && typeof Element.prototype.scrollIntoView !== 'function') {
  Element.prototype.scrollIntoView = function () {}
}

beforeEach(() => {
  navigateMock.mockReset()
  setHooks()
})

describe('CommandPalette — visibility', () => {
  it('renders nothing when open=false', () => {
    renderPalette(<CommandPalette open={false} onOpenChange={vi.fn()} />)
    expect(screen.queryByPlaceholderText(/Type to jump/i)).not.toBeInTheDocument()
  })

  it('renders the input when open=true', () => {
    renderPalette(<CommandPalette open={true} onOpenChange={vi.fn()} />)
    expect(screen.getByPlaceholderText(/Type to jump/i)).toBeInTheDocument()
  })

  it('renders route commands with their paths', () => {
    renderPalette(<CommandPalette open={true} onOpenChange={vi.fn()} />)
    // "Machines" / "Agents" also appear as group headings, so assert via the
    // path column which is unique to the routes group.
    expect(screen.getByText('/')).toBeInTheDocument()
    expect(screen.getByText('/machines')).toBeInTheDocument()
    expect(screen.getByText('/agents')).toBeInTheDocument()
    expect(screen.getByText('/settings')).toBeInTheDocument()
  })
})

describe('CommandPalette — fuzzy match + navigate', () => {
  it('typing a route name filters and Enter navigates + closes', async () => {
    const user = userEvent.setup()
    const onOpenChange = vi.fn()
    renderPalette(<CommandPalette open={true} onOpenChange={onOpenChange} />)

    const input = screen.getByPlaceholderText(/Type to jump/i)
    await user.type(input, 'sett')
    // The "Settings" command should be the only visible match in the routes group.
    expect(screen.getByText('Settings')).toBeInTheDocument()
    await user.keyboard('{Enter}')

    expect(navigateMock).toHaveBeenCalledWith('/settings')
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it('selecting an agent navigates to /agents/<id>', async () => {
    setHooks({ agents: [makeAgent('chewie'), makeAgent('han')] })
    const user = userEvent.setup()
    const onOpenChange = vi.fn()
    renderPalette(<CommandPalette open={true} onOpenChange={onOpenChange} />)

    const input = screen.getByPlaceholderText(/Type to jump/i)
    await user.type(input, 'chew')
    await user.keyboard('{Enter}')

    expect(navigateMock).toHaveBeenCalledWith('/agents/chewie')
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it('selecting a machine navigates to /machines/<id>', async () => {
    setHooks({ machines: [makeMachine('razer')] })
    const user = userEvent.setup()
    const onOpenChange = vi.fn()
    renderPalette(<CommandPalette open={true} onOpenChange={onOpenChange} />)

    const input = screen.getByPlaceholderText(/Type to jump/i)
    await user.type(input, 'raz')
    await user.keyboard('{Enter}')

    expect(navigateMock).toHaveBeenCalledWith('/machines/razer')
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it('shows the empty state when nothing matches', async () => {
    const user = userEvent.setup()
    renderPalette(<CommandPalette open={true} onOpenChange={vi.fn()} />)

    const input = screen.getByPlaceholderText(/Type to jump/i)
    await user.type(input, 'zzzznopenopezzzz')

    expect(screen.getByText(/No matches/i)).toBeInTheDocument()
  })
})

describe('CommandPalette — Esc closes', () => {
  it('Escape calls onOpenChange(false)', () => {
    const onOpenChange = vi.fn()
    renderPalette(<CommandPalette open={true} onOpenChange={onOpenChange} />)
    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    })
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })
})

describe('CommandPalette — dialog semantics', () => {
  it('panel is announced as a modal dialog with the title as its label', () => {
    renderPalette(<CommandPalette open={true} onOpenChange={vi.fn()} />)
    const dialog = screen.getByRole('dialog')
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    expect(dialog).toHaveAccessibleName('Command palette')
  })
})

describe('CommandPalette — loading states', () => {
  it('shows agent loading message while useAgents pending', () => {
    setHooks({ agentsLoading: true })
    renderPalette(<CommandPalette open={true} onOpenChange={vi.fn()} />)
    expect(screen.getByText(/Loading agents/i)).toBeInTheDocument()
  })

  it('shows machine loading message while useMachines pending', () => {
    setHooks({ machinesLoading: true })
    renderPalette(<CommandPalette open={true} onOpenChange={vi.fn()} />)
    expect(screen.getByText(/Loading machines/i)).toBeInTheDocument()
  })
})
