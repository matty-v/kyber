import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ArchiveJob } from '../lib/types'

const { imports, cancelImport } = vi.hoisted(() => ({
  imports: { data: [] as ArchiveJob[] },
  cancelImport: { mutate: vi.fn(), isPending: false },
}))
vi.mock('../hooks/useAPI', () => ({
  useAgentImports: () => imports,
  useCancelAgentImport: () => cancelImport,
}))

import { RestoreStatusCard } from './RestoreStatusCard'

const running: ArchiveJob = {
  id: 'imp1', kind: 'import', agent: 'leia', state: 'running', step: 'restoring', message: 'Restoring the disk',
  bytesDone: 0, createdAt: '2026-09-22T12:00:00Z', cancelable: true, downloadable: false,
  cutover: ['Telegram was enabled on han. Enable it on the new agent only after disabling it on han.'],
}

describe('RestoreStatusCard', () => {
  beforeEach(() => { imports.data = []; vi.clearAllMocks() })

  it('renders nothing for an agent that was not restored', () => {
    const { container } = render(<RestoreStatusCard agentName="leia" capability={{ available: true }} />)
    expect(container).toBeEmptyDOMElement()
  })

  it.each(['queued', 'running'] as const)('shows %s progress and cancels without rendering migration instructions', async (state) => {
    imports.data = [{ ...running, state }]
    render(<RestoreStatusCard agentName="leia" capability={{ available: true }} />)
    expect(screen.getByTestId('restore-state')).toHaveTextContent('Restoring the disk')
    expect(screen.queryByTestId('restore-cutover')).toBeNull()
    expect(screen.queryByText(running.cutover![0])).toBeNull()
    expect(screen.getByText(/source agent is never touched/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Cancel restore' }))
    expect(cancelImport.mutate).toHaveBeenCalledWith('imp1')
  })

  it.each(['completed', 'canceled', 'expired'] as const)('hides the restore card after %s', (state) => {
    imports.data = [{ ...running, state, restored: true, cancelable: false, message: 'Restored; agent is NeedsAuth' }]
    const { container } = render(<RestoreStatusCard agentName="leia" capability={{ available: true }} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('retains failed-restore errors without showing stored checklist text', () => {
    const text = `Harness login files are never restored (${'session.json, '.repeat(500)}). Authorize the new agent.`
    imports.data = [{ ...running, state: 'failed', cancelable: false, message: 'Restore failed', error: 'Disk verification failed', cutover: [text] }]
    render(<RestoreStatusCard agentName="leia" capability={{ available: true }} />)
    expect(screen.getByTestId('restore-state')).toHaveTextContent('Restore failed')
    expect(screen.getByText('Disk verification failed')).toBeVisible()
    expect(screen.queryByText(text)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Cancel restore' })).toBeNull()
  })
})
