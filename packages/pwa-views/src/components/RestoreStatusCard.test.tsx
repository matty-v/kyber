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

  it('shows progress, the cutover checklist, and cancels', async () => {
    imports.data = [running]
    render(<RestoreStatusCard agentName="leia" capability={{ available: true }} />)
    expect(screen.getByTestId('restore-state')).toHaveTextContent('Restoring the disk')
    expect(screen.getByTestId('restore-cutover')).toHaveTextContent('Telegram was enabled on han')
    expect(screen.getByText(/source agent is never touched/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Cancel restore' }))
    expect(cancelImport.mutate).toHaveBeenCalledWith('imp1')
  })

  it('does not offer cancel once the disk is restored', () => {
    imports.data = [{ ...running, state: 'completed', restored: true, cancelable: false, message: 'Restored; agent is NeedsAuth' }]
    render(<RestoreStatusCard agentName="leia" capability={{ available: true }} />)
    expect(screen.queryByRole('button', { name: 'Cancel restore' })).toBeNull()
    expect(screen.getByTestId('restore-state')).toHaveTextContent('NeedsAuth')
  })
})
