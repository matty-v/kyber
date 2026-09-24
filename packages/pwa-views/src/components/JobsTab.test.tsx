import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AgentJob } from '../lib/types'

const { agent, patchJobs, runJob } = vi.hoisted(() => ({
  agent: { data: { jobs: [] as AgentJob[] } },
  patchJobs: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  runJob: { mutate: vi.fn(), isPending: false },
}))
vi.mock('../hooks/useAPI', () => ({
  useAgent: () => ({ data: agent.data, isLoading: false, error: null }),
  usePatchAgentJobs: () => patchJobs,
  useRunAgentJob: () => runJob,
}))

import { JobsTab } from './JobsTab'

const digest: AgentJob = { name: 'digest', schedule: '0 9 * * *', prompt: 'Send the digest.' }
const sweep: AgentJob = { name: 'sweep', schedule: '*/30 * * * *', prompt: 'Sweep.', paused: true }

describe('JobsTab paused jobs', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    agent.data = { jobs: [digest, sweep] }
  })

  it('marks paused jobs and refuses to run them by hand', () => {
    render(<JobsTab agentName="han" />)
    // Mobile cards and the desktop table both render in jsdom.
    expect(screen.getAllByTestId('job-paused')).toHaveLength(2)
    const runButtons = screen.getAllByTitle(/Run now|Paused; resume the job to run it/)
    const paused = runButtons.filter((b) => b.getAttribute('title')?.startsWith('Paused'))
    expect(paused).toHaveLength(2)
    paused.forEach((b) => expect(b).toBeDisabled())
  })

  it('pauses and resumes a job through PATCH jobs', async () => {
    render(<JobsTab agentName="han" />)
    await userEvent.click(screen.getAllByTitle('Pause')[0])
    expect(patchJobs.mutate).toHaveBeenLastCalledWith({ name: 'han', jobs: [{ ...digest, paused: true }, sweep] })
    await userEvent.click(screen.getAllByTitle('Resume')[0])
    expect(patchJobs.mutate).toHaveBeenLastCalledWith({ name: 'han', jobs: [digest, { ...sweep, paused: undefined }] })
  })

  it('editing a paused job keeps it paused', async () => {
    patchJobs.mutateAsync.mockResolvedValue({})
    render(<JobsTab agentName="han" />)
    await userEvent.click(screen.getAllByTitle('Edit')[1])
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(patchJobs.mutateAsync).toHaveBeenCalledWith({ name: 'han', jobs: [digest, expect.objectContaining({ name: 'sweep', paused: true })] })
  })
})
