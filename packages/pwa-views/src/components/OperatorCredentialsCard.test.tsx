import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { OperatorCredentialsCard } from './OperatorCredentialsCard'
import type { OperatorCredentialInventory } from '../lib/types'

const state = vi.hoisted(() => ({
  api: { getOperatorCredentials: vi.fn() },
  cluster: { id: 'local', baseURL: '/', apiKey: '', name: 'dev', version: '', capabilities: [] },
}))
vi.mock('../lib/cluster-context', () => ({ useCluster: () => state.cluster }))
vi.mock('../lib/api', () => ({ createApiClient: () => state.api }))

const inventory: OperatorCredentialInventory = {
  observedAt: '2026-10-23T13:00:00Z', inventoryScope: 'current-control-plane', configurationAvailable: true,
  usageAvailable: false, warnings: [], credentials: [
    { id: 'admin', name: 'installation-admin', displayName: 'Installation administrator', accessLevel: 'administrator', state: 'accepted', source: 'installation', scopes: [], agentResources: ['*'], permissions: ['All operator actions on all Agents and Machines'], configured: true },
    { id: 'integration', name: 'jack-bridge', displayName: 'jack-bridge', accessLevel: 'restricted', state: 'accepted', source: 'configuration', scopes: ['tasks:create'], agentResources: ['kyber-system/jack'], permissions: ['Submit tasks'], configured: true },
    { id: 'inactive', name: 'configured-key', displayName: 'configured-key', accessLevel: 'restricted', state: 'configured-not-accepted', source: 'configuration', scopes: [], agentResources: [], permissions: [], configured: true },
  ],
}

afterEach(() => { cleanup(); vi.clearAllMocks() })

describe('operator credential inventory', () => {
  it('explains actual access and unavailable history without exposing key controls', async () => {
    state.api.getOperatorCredentials.mockResolvedValue(inventory)
    render(<OperatorCredentialsCard><button>Administrator rotation</button></OperatorCredentialsCard>)
    expect(await screen.findByText('Installation administrator')).toBeInTheDocument()
    expect(screen.getByText('jack-bridge')).toBeInTheDocument()
    expect(screen.getByText('Submit tasks')).toBeInTheDocument()
    expect(screen.getByText('Agent access: kyber-system/jack')).toBeInTheDocument()
    expect(screen.getByText('Configured, not active')).toBeInTheDocument()
    expect(screen.getByText(/Usage history is not available yet/)).toBeInTheDocument()
    expect(screen.queryByText(/Never used/i)).not.toBeInTheDocument()
    expect(screen.queryByText('legacy')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Administrator rotation' })).toBeInTheDocument()
  })

  it.each([403, 404])('shows access/compatibility errors rather than an empty inventory (%s)', async (status) => {
    state.api.getOperatorCredentials.mockRejectedValue({ status })
    render(<OperatorCredentialsCard><button>Administrator rotation</button></OperatorCredentialsCard>)
    expect(await screen.findByRole('alert')).toHaveTextContent(status === 403 ? 'Administrator access is required' : 'not available on this installation')
    expect(screen.queryByText('No accepted credentials were reported.')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Administrator rotation' })).not.toBeInTheDocument()
  })

  it('clears previously loaded administrator metadata and controls on denial', async () => {
    state.api.getOperatorCredentials.mockResolvedValueOnce(inventory).mockRejectedValue({ status: 403 })
    const user = userEvent.setup()
    render(<OperatorCredentialsCard><button>Administrator rotation</button></OperatorCredentialsCard>)
    await screen.findByText('jack-bridge')
    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Administrator access is required'))
    expect(screen.queryByText('jack-bridge')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Administrator rotation' })).not.toBeInTheDocument()
  })
})
