import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import type { Agent } from '../lib/types'

vi.mock('./ExecTerminal', () => ({
  ExecTerminal: ({ name, mode }: { name: string; mode?: string }) => (
    <div data-testid="exec" data-name={name} data-mode={mode} />
  ),
}))
vi.mock('../hooks/usePageVisible', () => ({ usePageVisible: () => true }))

import { TerminalPeek } from './TerminalPeek'

const NOW = Date.parse('2026-07-06T12:00:00Z')
const a = (id: string, secsAgo?: number): Agent => ({
  id, phase: 'Running', machine: 'm', runtime: 'claude-code', model: 'claude-sonnet-4-6',
  resources: { cpu: '1', memory: '1Gi', disk: '10Gi' },
  status: {} as Agent['status'], createdAt: '2026-07-06T10:00:00Z',
  activity: secsAgo === undefined ? undefined : { lastActivityAt: new Date(NOW - secsAgo * 1000).toISOString() },
})

describe('TerminalPeek', () => {
  it('defaults the attach to the most-recently-active agent in live mode', () => {
    render(<TerminalPeek agents={[a('old', 300), a('fresh', 10)]} />)
    const exec = screen.getByTestId('exec')
    expect(exec).toHaveAttribute('data-name', 'fresh')
    expect(exec).toHaveAttribute('data-mode', 'attach')
  })
  it('renders an empty state with no agents', () => {
    render(<TerminalPeek agents={[]} />)
    expect(screen.getByText('No agents to watch')).toBeInTheDocument()
  })
})
