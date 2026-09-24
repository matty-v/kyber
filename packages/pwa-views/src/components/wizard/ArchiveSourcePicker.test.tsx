import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ArchiveJob } from '../../lib/types'
import { initialWizardState, type WizardState } from './types'
import { isBasicsValid } from './validation'

const { archives, uploadArchive, deleteArchive } = vi.hoisted(() => ({
  archives: { data: [] as ArchiveJob[], isLoading: false },
  uploadArchive: { mutate: vi.fn(), isPending: false },
  deleteArchive: { mutate: vi.fn(), isPending: false },
}))
vi.mock('../../hooks/useAPI', () => ({
  useArchives: () => archives,
  useArchiveUpload: () => ({ data: undefined }),
  useUploadArchive: () => uploadArchive,
  useDeleteArchive: () => deleteArchive,
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

const configuredExport: ArchiveJob = {
  ...exportJob,
  id: 'exp2',
  summary: {
    ...exportJob.summary!,
    source: {
      agent: 'vault', runtime: 'codex',
      config: {
        version: 2, runtime: 'codex', model: 'gpt-5-codex', startupPrompt: 'Carry on.',
        resources: { cpu: '2', memory: '4Gi' }, soulDescription: 'Scruffy.', identityRepo: 'matty-v/vault-id',
        jobs: [{ name: 'digest', schedule: '0 9 * * *', prompt: 'p' }],
        inboundBindings: ['github'],
        userSecrets: [{ key: 'API_TOKEN', kind: 'kv', size: 8 }],
      },
    },
    login: ['agentroot/home/kyber/.codex/auth.json'],
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

  it('prefills the create fields from the archived config', async () => {
    archives.data = [configuredExport]
    const { get } = renderPicker({ identityRepoModes: ['none', 'existing'] })
    await userEvent.click(screen.getByLabelText('A disk archive'))
    await userEvent.selectOptions(screen.getByLabelText('Archive'), 'exp2')
    const s = get()
    expect([s.model, s.startupPrompt, s.cpu, s.memory, s.soulDescription]).toEqual(['gpt-5-codex', 'Carry on.', '2', '4Gi', 'Scruffy.'])
    expect([s.identityRepoMode, s.identityRepoExisting]).toEqual(['existing', 'matty-v/vault-id'])
    expect(s.archiveApply).toEqual({ config: true, jobs: 'paused', bindings: 'disabled', secrets: 'copy-if-local' })
  })

  it('does not link an identity repo the installation cannot link', async () => {
    archives.data = [configuredExport]
    const { get } = renderPicker({ identityRepoModes: ['none'] })
    await userEvent.click(screen.getByLabelText('A disk archive'))
    await userEvent.selectOptions(screen.getByLabelText('Archive'), 'exp2')
    expect(get().identityRepoMode).toBe('none')
  })

  it('lists what the archive carries and maps the choices to apply', async () => {
    archives.data = [configuredExport]
    const { get } = renderPicker()
    await userEvent.click(screen.getByLabelText('A disk archive'))
    await userEvent.selectOptions(screen.getByLabelText('Archive'), 'exp2')
    expect(screen.getByTestId('archive-apply-jobs')).toHaveTextContent('digest')
    expect(screen.getByTestId('archive-apply-bindings')).toHaveTextContent('Restored disabled, each with a new signing secret')
    expect(screen.getByTestId('archive-apply-secrets')).toHaveTextContent('API_TOKEN (kv)')
    expect(screen.getByTestId('archive-apply-login')).toHaveTextContent('.codex/auth.json')
    await userEvent.selectOptions(screen.getByLabelText('Scheduled jobs'), 'skip')
    await userEvent.selectOptions(screen.getByLabelText('User secrets'), 'skip')
    expect(get().archiveApply).toMatchObject({ jobs: 'skip', bindings: 'disabled', secrets: 'skip' })
    await userEvent.click(screen.getByLabelText(/Apply vault's configuration/))
    expect(get().archiveApply.config).toBe(false)
    expect(screen.getByLabelText('Webhook bindings')).toBeDisabled()
  })

  it('warns when the archive predates full configuration capture', async () => {
    archives.data = [{ ...configuredExport, summary: { ...configuredExport.summary!, source: { agent: 'old', runtime: 'codex', config: { jobs: [] } } } }]
    renderPicker()
    await userEvent.click(screen.getByLabelText('A disk archive'))
    await userEvent.selectOptions(screen.getByLabelText('Archive'), 'exp2')
    expect(screen.getByTestId('archive-apply')).toHaveTextContent('predates full configuration capture')
  })

  it('deletes the selected archive after confirmation', async () => {
    const { get } = renderPicker()
    await userEvent.click(screen.getByLabelText('A disk archive'))
    await userEvent.selectOptions(screen.getByLabelText('Archive'), 'exp1')
    await userEvent.click(screen.getByLabelText('Delete archive'))
    expect(deleteArchive.mutate).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Delete' }))
    expect(deleteArchive.mutate).toHaveBeenCalledWith({ agent: 'han', id: 'exp1' }, expect.anything())
    const opts = deleteArchive.mutate.mock.calls[0][1] as { onSuccess: () => void }
    opts.onSuccess()
    expect(get().archiveSource?.exportId).toBeUndefined()
  })

  it('computes the same minimum disk as the control plane', () => {
    expect(requiredDiskGi(0)).toBe(1)
    expect(requiredDiskGi(10 * 1024 ** 3)).toBe(12)
  })
})
