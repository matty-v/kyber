import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ArchiveJob } from '../lib/types'

const { exportsQuery, startExport, cancelExport, downloadLink, deleteArchive } = vi.hoisted(() => ({
  deleteArchive: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  exportsQuery: { data: [] as ArchiveJob[], isLoading: false },
  startExport: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  cancelExport: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  downloadLink: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
}))

vi.mock('../hooks/useAPI', () => ({
  useAgentExports: () => exportsQuery,
  useStartAgentExport: () => startExport,
  useCancelAgentExport: () => cancelExport,
  useExportDownloadLink: () => downloadLink,
  useDeleteArchive: () => deleteArchive,
}))

import { DiskExportCard } from './DiskExportCard'

const available = { available: true, store: 'kyber-archive-store' }

const completed: ArchiveJob = {
  id: 'j1', kind: 'export', agent: 'han', state: 'completed', message: 'Ready to download',
  bytesDone: 2048, sizeBytes: 2048, createdAt: '2026-09-22T12:00:00Z', expiresAt: '2026-09-25T12:00:00Z',
  cancelable: false, downloadable: true,
  summary: {
    formatVersion: 'kyber.io/agent-disk-archive/v1', exportedAt: '2026-09-22T12:01:00Z',
    source: { agent: 'han', runtime: 'claude-code' },
    mounts: [
      { path: '/persist', kind: 'persistentVolumeClaim', archived: true, reason: 'the agent disk' },
      { path: '/var/run/secrets/kyber', kind: 'secret', archived: false, reason: 'Kubernetes Secrets are never exported' },
    ],
    totals: { entries: 3, files: 2, dirs: 1, symlinks: 0, bytes: 2048 },
    sensitive: ['agentroot/home/kyber/.claude/.credentials.json'],
  },
}

describe('DiskExportCard', () => {
  beforeEach(() => {
    exportsQuery.data = []
    vi.clearAllMocks()
  })

  it('deletes a completed export after confirmation (MAT-90)', async () => {
    exportsQuery.data = [completed]
    render(<DiskExportCard agentName="han" phase="Running" capability={available} />)
    await userEvent.click(screen.getByRole('button', { name: 'Delete' }))
    expect(deleteArchive.mutate).not.toHaveBeenCalled()
    expect(screen.getByText(/Download links stop working/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Delete export' }))
    expect(deleteArchive.mutate).toHaveBeenCalledWith({ agent: 'han', id: 'j1' }, expect.anything())
  })

  it('offers no delete while an export runs', () => {
    exportsQuery.data = [{ ...completed, state: 'running', cancelable: true, downloadable: false }]
    render(<DiskExportCard agentName="han" phase="Running" capability={available} />)
    expect(screen.queryByRole('button', { name: 'Delete' })).not.toBeInTheDocument()
  })

  it('lists harness login files separately as never restored', async () => {
    exportsQuery.data = [{ ...completed, summary: { ...completed.summary!, sensitive: [], login: ['agentroot/home/kyber/.claude/.credentials.json'] } }]
    render(<DiskExportCard agentName="han" phase="Running" capability={available} />)
    await userEvent.click(screen.getByRole('button', { name: 'Contents' }))
    expect(screen.getByTestId('export-contents')).toHaveTextContent('Harness login (never restored)')
  })

  it('explains why export is unavailable', () => {
    render(<DiskExportCard agentName="han" phase="Running" capability={{ available: false, unavailableReason: 'disk archives need PostgreSQL' }} />)
    expect(screen.getByTestId('export-unavailable')).toHaveTextContent('PostgreSQL')
    expect(screen.getByRole('button', { name: 'Export disk' })).toBeDisabled()
  })

  it('confirms before starting, warning that the agent stops', async () => {
    render(<DiskExportCard agentName="han" phase="Running" capability={available} />)
    await userEvent.click(screen.getByRole('button', { name: 'Export disk' }))
    expect(screen.getByText(/stopped while its disk is archived/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Export' }))
    expect(startExport.mutate).toHaveBeenCalledWith('han')
  })

  it('does not offer export from a transient phase or while one is running', () => {
    const { rerender } = render(<DiskExportCard agentName="han" phase="Starting" capability={available} />)
    expect(screen.getByRole('button', { name: 'Export disk' })).toBeDisabled()
    exportsQuery.data = [{ ...completed, id: 'j2', state: 'running', message: 'Archiving the disk', downloadable: false, cancelable: true, summary: undefined }]
    rerender(<DiskExportCard agentName="han" phase="Stopped" capability={available} />)
    expect(screen.getByRole('button', { name: 'Export disk' })).toBeDisabled()
    expect(screen.getByText('Archiving the disk')).toBeInTheDocument()
  })

  it('cancels a running export', async () => {
    exportsQuery.data = [{ ...completed, id: 'j2', state: 'running', message: 'Archiving the disk', downloadable: false, cancelable: true, summary: undefined }]
    render(<DiskExportCard agentName="han" phase="Stopped" capability={available} />)
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(cancelExport.mutate).toHaveBeenCalledWith({ name: 'han', id: 'j2' })
  })

  it('downloads a completed export through a short-lived link', async () => {
    exportsQuery.data = [completed]
    downloadLink.mutateAsync.mockResolvedValue({ url: 'http://kyber/api/v1/archive-downloads/tok', filename: 'han-disk.zip', expiresAt: '' })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    render(<DiskExportCard agentName="han" phase="Running" capability={available} />)
    await userEvent.click(screen.getByRole('button', { name: 'Download' }))
    expect(downloadLink.mutateAsync).toHaveBeenCalledWith({ name: 'han', id: 'j1' })
    expect(click).toHaveBeenCalled()
    click.mockRestore()
  })

  it('shows what is and is not in the archive', async () => {
    exportsQuery.data = [completed]
    render(<DiskExportCard agentName="han" phase="Running" capability={available} />)
    await userEvent.click(screen.getByRole('button', { name: 'Contents' }))
    const contents = screen.getByTestId('export-contents')
    expect(contents).toHaveTextContent('Included /persist')
    expect(contents).toHaveTextContent('Not included /var/run/secrets/kyber')
    expect(contents).toHaveTextContent('.credentials.json')
  })

  it('shows a failed export with its reason', () => {
    exportsQuery.data = [{ ...completed, state: 'failed', message: 'Failed', error: 'agent pod did not stop within the pause limit', downloadable: false, summary: undefined }]
    render(<DiskExportCard agentName="han" phase="Running" capability={available} />)
    expect(screen.getByText(/did not stop within the pause limit/)).toBeInTheDocument()
  })
})
