import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ArchiveSummary } from '../../lib/types'
import { initialWizardState } from './types'
import { ArchiveApplySection } from './ArchiveApplySection'

const base: ArchiveSummary = {
  formatVersion: 'kyber.io/agent-disk-archive/v1', exportedAt: '2026-11-21T00:00:00Z',
  source: { agent: 'source', runtime: 'claude-code' }, mounts: [],
  totals: { entries: 0, files: 0, dirs: 0, symlinks: 0, bytes: 0 },
}

describe('ArchiveApplySection login details', () => {
  it.each([
    ['claude-code', '.claude/sessions/'],
    ['codex', '.codex/auth.json'],
    ['hermes', '.hermes/auth.json'],
  ])('keeps %s file inventories collapsed but fully accessible', async (runtime, path) => {
    const login = Array.from({ length: 500 }, (_, i) => `agentroot/home/kyber/${path}${i}`)
    render(<ArchiveApplySection summary={{ ...base, source: { ...base.source, runtime }, login }} state={initialWizardState([])} set={vi.fn()} />)
    expect(screen.getByText(/500 harness login files will not be restored/)).toBeVisible()
    expect(screen.getByText(/new agent signs in with its own credential/)).toBeVisible()
    const last = screen.getByText(login[499])
    expect(last).not.toBeVisible()
    await userEvent.click(screen.getByText('Show excluded login files'))
    expect(last).toBeVisible()
    expect(screen.getByRole('list', { name: 'Excluded login files' })).toHaveClass('max-h-48', 'overflow-y-auto')
    await userEvent.click(screen.getByText('Show excluded login files'))
    expect(last).not.toBeVisible()
  })

  it('omits the login block when there are no excluded files', () => {
    render(<ArchiveApplySection summary={base} state={initialWizardState([])} set={vi.fn()} />)
    expect(screen.queryByTestId('archive-apply-login')).toBeNull()
  })
})
