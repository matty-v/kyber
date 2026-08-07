import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SessionSection } from './SessionSection'
import type { Session } from '../lib/transcript'

const session: Session = {
  id: 'sess-1',
  startedAt: '2026-07-13T18:42:41Z',
  endedAt: '2026-07-13T18:42:59Z',
  firstUserText: 'Weather in Springfield?',
  lastAssistantText: 'It is sunny.',
  turns: [
    { kind: 'user', ts: '2026-07-13T18:42:41Z', text: 'Weather in Springfield?' },
    { kind: 'assistant', ts: '2026-07-13T18:42:59Z', text: 'It is sunny.' },
  ],
}

describe('SessionSection', () => {
  it('shows the turns when defaultExpanded', () => {
    render(<SessionSection session={session} defaultExpanded />)
    expect(screen.getByText('It is sunny.')).toBeInTheDocument()
  })

  it('hides the turns when collapsed and shows a first-message preview', () => {
    render(<SessionSection session={session} defaultExpanded={false} />)
    expect(screen.queryByText('It is sunny.')).not.toBeInTheDocument()
    // The collapsed header previews the first user message.
    expect(screen.getByText(/Weather in Springfield\?/)).toBeInTheDocument()
  })

  it('expands on header click', async () => {
    const user = userEvent.setup()
    render(<SessionSection session={session} defaultExpanded={false} />)
    expect(screen.queryByText('It is sunny.')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /turns/i }))
    expect(screen.getByText('It is sunny.')).toBeInTheDocument()
  })

  it('renders the turn count and start time in the header', () => {
    render(<SessionSection session={session} defaultExpanded={false} />)
    expect(screen.getByText(/2 turns/)).toBeInTheDocument()
  })
})
