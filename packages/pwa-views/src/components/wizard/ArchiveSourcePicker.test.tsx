import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ArchiveJob } from '../../lib/types'
import { initialWizardState, type WizardState } from './types'
import { isBasicsValid } from './validation'

const { archives, uploadArchive } = vi.hoisted(() => ({
  archives: { data: [] as ArchiveJob[], isLoading: false },
  uploadArchive: { mutate: vi.fn(), isPending: false },
}))
vi.mock('../../hooks/useAPI', () => ({
  useArchives: () => archives,
  useArchiveUpload: () => ({ data: undefined }),
  useUploadArchive: () => uploadArchive,
}))

import { ArchiveSourcePicker, requiredDiskGi } from './ArchiveSourcePicker'

const exportJob: ArchiveJob = {
  id: 'exp1', kind: 'export', agent: 'han', state: 'completed', bytesDone: 0, sizeBytes: 5 * 1024 ** 3,
  createdAt: '2026-09-22T12:00:00Z', cancelable: false, downloadable: true,
  summary: {
    formatVersion: 'kyber.io/agent-disk-archive/v1', exportedAt: '2026-09-22T12:00:00Z',
    source: { agent: 'han', runtime: 'codex' }, mounts: [],
    totals: { entries: 10, files: 8, dirs: 2, symlinks: 0, bytes: 20 * 1024 ** 3 },
  },
}

function renderPicker(initial: Partial<WizardState> = {}) {
  let state: WizardState = { ...initialWizardState([]), name: 'leia', machine: 'm1', disk: '10Gi', runtimes: [
    { id: 'claude-code', authModes: [] }, { id: 'codex', authModes: [] },
  ] as unknown as WizardState['runtimes'], ...initial }
  const set = vi.fn((k: keyof WizardState, v: unknown) => { state = { ...state, [k]: v } as WizardState; rerender() })
  const view = render(<ArchiveSourcePicker state={state} set={set as never} capability={{ available: true }} />)
  function rerender() { view.rerender(<ArchiveSourcePicker state={state} set={set as never} capability={{ available: true }} />) }
  return { set, get: () => state }
}

describe('ArchiveSourcePicker', () => {
  beforeEach(() => { archives.data = [exportJob]; vi.clearAllMocks() })

  it('is hidden where disk archives are unavailable', () => {
    const { container } = render(<ArchiveSourcePicker state={initialWizardState([])} set={vi.fn() as never} capability={{ available: false }} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('picking an export adopts its runtime and a disk large enough for it', async () => {
    const { get } = renderPicker()
    await userEvent.click(screen.getByLabelText('A disk archive'))
    expect(isBasicsValid(get()).ok).toBe(false)
    await userEvent.selectOptions(screen.getByLabelText('Archive'), 'exp1')
    expect(get().archiveSource?.exportId).toBe('exp1')
    expect(get().runtime).toBe('codex')
    expect(get().disk).toBe(`${requiredDiskGi(20 * 1024 ** 3)}Gi`)
    expect(isBasicsValid(get()).ok).toBe(true)
    expect(screen.getByTestId('archive-summary')).toHaveTextContent('From han (codex)')
  })

  it('leaves credential files out unless the operator opts in', async () => {
    const { get } = renderPicker()
    await userEvent.click(screen.getByLabelText('A disk archive'))
    await userEvent.selectOptions(screen.getByLabelText('Archive'), 'exp1')
    expect(get().keepCredentialFiles).toBe(false)
    await userEvent.click(screen.getByLabelText(/Also restore files that look like credentials/))
    expect(get().keepCredentialFiles).toBe(true)
  })

  it('switching back to an empty disk clears the source', async () => {
    const { get } = renderPicker()
    await userEvent.click(screen.getByLabelText('A disk archive'))
    await userEvent.click(screen.getByLabelText('An empty disk'))
    expect(get().archiveSource).toBeUndefined()
  })

  it('computes the same minimum disk as the control plane', () => {
    expect(requiredDiskGi(0)).toBe(1)
    expect(requiredDiskGi(10 * 1024 ** 3)).toBe(12)
  })
})
