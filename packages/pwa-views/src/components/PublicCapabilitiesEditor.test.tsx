import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { Agent } from '../lib/types'
import { PublicCapabilitiesEditor } from './PublicCapabilitiesEditor'

const mutate = vi.fn()
vi.mock('../hooks/useAPI', () => ({
  useSetPublicCapabilities: () => ({ mutate, isPending: false }),
  useAgentSkills: () => ({ data: { skills: [{ name: 'private-deploy-skill' }] } }),
}))

const agent: Agent = {
  id: 'reviewer', phase: 'Running', machine: 'worker', runtime: 'codex', model: '', resources: { cpu: '1', memory: '1Gi', disk: '10Gi' }, status: { phase: 'Running' },
  publicCapabilities: {
    schemaVersion: 'v1alpha1', identity: { displayName: 'Reviewer', description: 'Reviews code.' },
    capabilities: [{ id: 'code-review', version: '1', name: 'Review code', description: 'Produces findings.', inputModes: ['text/plain'], outputModes: ['application/json'], evidence: { requiredSkills: ['private-deploy-skill'], runtimeAdapters: ['codex'] } }],
  },
  publicCapabilitiesStatus: { capabilities: [{ id: 'code-review', availability: 'available' }] },
}

describe('PublicCapabilitiesEditor', () => {
  it('shows an exact preview without private evidence and requires an explicit update', async () => {
    render(<PublicCapabilitiesEditor agent={agent} />)
    const preview = screen.getByText('Exact public preview').parentElement?.querySelector('pre')?.textContent ?? ''
    expect(preview).toContain('code-review')
    expect(preview).toContain('available')
    expect(preview).not.toContain('private-deploy-skill')
    expect(preview).not.toContain('runtimeAdapters')
    await userEvent.click(screen.getByRole('button', { name: 'Update publication' }))
    expect(mutate).toHaveBeenCalledWith({ name: 'reviewer', publicCapabilities: agent.publicCapabilities })
  })
})
