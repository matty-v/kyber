import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { ConversationTurn } from './ConversationTurn'

describe('ConversationTurn', () => {
  it('renders a user turn with its text', () => {
    render(<ConversationTurn turn={{ kind: 'user', ts: '2026-07-13T18:00:00Z', text: 'hello' }} />)
    expect(screen.getByText('hello')).toBeInTheDocument()
  })
  it('renders an assistant turn with its text', () => {
    render(<ConversationTurn turn={{ kind: 'assistant', ts: '2026-07-13T18:00:00Z', text: 'hi back' }} />)
    expect(screen.getByText('hi back')).toBeInTheDocument()
  })
  it('renders a tool turn via ToolCall', () => {
    render(<ConversationTurn turn={{ kind: 'tool', ts: '2026-07-13T18:00:00Z', name: 'WebSearch', input: {} }} />)
    expect(screen.getByText('WebSearch')).toBeInTheDocument()
  })
  it('renders a thinking turn via ThinkingBlock', () => {
    render(<ConversationTurn turn={{ kind: 'thinking', ts: '2026-07-13T18:00:00Z', text: 'hmm' }} />)
    expect(screen.getByRole('button', { name: /thinking/i })).toBeInTheDocument()
  })
  it('renders a channel chip for a user turn with a channel', () => {
    render(
      <ConversationTurn
        turn={{ kind: 'user', ts: '2026-07-13T18:00:00Z', text: 'hi', channel: { source: 'plugin:telegram:telegram' } }}
      />,
    )
    expect(screen.getByText(/via Telegram/i)).toBeInTheDocument()
  })
})
