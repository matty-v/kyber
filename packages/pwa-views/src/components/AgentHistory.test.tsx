import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { AgentHistory } from './AgentHistory'
import { parseTranscript } from '../lib/transcript'
import { twoSessionJsonl } from '../lib/transcript-fixtures'

vi.mock('../hooks/useAPI', () => ({ useAgentTranscript: vi.fn() }))

import * as useAPIModule from '../hooks/useAPI'

function mockTranscript(overrides: Record<string, unknown> = {}) {
  vi.mocked(useAPIModule.useAgentTranscript).mockReturnValue({
    data: { sessions: parseTranscript(twoSessionJsonl), truncated: false },
    isLoading: false,
    error: null,
    refetch: vi.fn(),
    isFetching: false,
    ...overrides,
  } as unknown as ReturnType<typeof useAPIModule.useAgentTranscript>)
}

beforeEach(() => mockTranscript())

describe('AgentHistory', () => {
  it('surfaces the latest exchange in an expanded Recent conversation section', () => {
    render(<AgentHistory agentName="echo" />)
    expect(screen.getByText(/recent conversation/i)).toBeInTheDocument()
    // The newest session's content is visible via Recent without expanding anything
    // ("Springfield" appears in both the question and the reply).
    expect(screen.getAllByText(/Springfield/).length).toBeGreaterThan(0)
    expect(screen.getByText('WebSearch')).toBeInTheDocument()
  })

  it('keeps the full sessions collapsed by default', () => {
    render(<AgentHistory agentName="echo" />)
    // "Will do." lives only in the older (non-recent) session, which starts
    // collapsed — so it must not be on screen until that session is expanded.
    expect(screen.queryByText('Will do.')).not.toBeInTheDocument()
    // Exactly one region is expanded on load: the Recent conversation section.
    const expanded = screen
      .getAllByRole('button')
      .filter((b) => b.getAttribute('aria-expanded') === 'true')
    expect(expanded).toHaveLength(1)
  })

  it('renders Refresh and Export controls', () => {
    render(<AgentHistory agentName="echo" />)
    expect(screen.getByRole('button', { name: /refresh/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /export/i })).toBeInTheDocument()
  })

  it('shows the session count', () => {
    render(<AgentHistory agentName="echo" />)
    expect(screen.getByText(/2 sessions/i)).toBeInTheDocument()
  })

  it('shows a truncation banner that matches what the server now keeps (newest, not earliest)', () => {
    mockTranscript({ data: { sessions: parseTranscript(twoSessionJsonl), truncated: true } })
    render(<AgentHistory agentName="echo" />)
    // kyber#669 flipped retention to newest-wins. The banner must not claim the
    // opposite — it told operators the recent activity was the part missing.
    expect(screen.getByText(/most recent activity backwards/i)).toBeInTheDocument()
    expect(screen.getByText(/earliest part of the window is[\s\S]*missing/i)).toBeInTheDocument()
    expect(screen.queryByText(/earliest part of the 7-day window/i)).not.toBeInTheDocument()
  })

  it('shows the empty state when there is no history', () => {
    mockTranscript({ data: { sessions: [], truncated: false } })
    render(<AgentHistory agentName="echo" />)
    expect(screen.getByText(/no conversation history/i)).toBeInTheDocument()
  })

  // kyber#669: the panel opens on 24h. A 7-day default was an 84.7 MB fetch for a
  // busy agent and the tab died before rendering.
  it('opens on a 24-hour window', () => {
    render(<AgentHistory agentName="echo" />)
    expect(useAPIModule.useAgentTranscript).toHaveBeenCalledWith('echo', 1)
    expect(screen.getByText(/last 24 hours/i)).toBeInTheDocument()
  })

  it('widens the window one step per "Load earlier" click, then stops offering it', () => {
    render(<AgentHistory agentName="echo" />)

    fireEvent.click(screen.getByRole('button', { name: /load earlier/i }))
    expect(useAPIModule.useAgentTranscript).toHaveBeenCalledWith('echo', 3)

    fireEvent.click(screen.getByRole('button', { name: /load earlier/i }))
    expect(useAPIModule.useAgentTranscript).toHaveBeenCalledWith('echo', 7)

    // 7 days is the widest step — the control is gone rather than re-fetching.
    expect(screen.queryByRole('button', { name: /load earlier/i })).not.toBeInTheDocument()
    expect(screen.getByText(/last 7 days/i)).toBeInTheDocument()
  })

  it('offers "Load earlier" from the empty state too', () => {
    mockTranscript({ data: { sessions: [], truncated: false } })
    render(<AgentHistory agentName="echo" />)
    // An agent idle for a day would otherwise look like it had no history at all,
    // with no way to look further back.
    expect(screen.getByRole('button', { name: /load earlier/i })).toBeInTheDocument()
  })
})
