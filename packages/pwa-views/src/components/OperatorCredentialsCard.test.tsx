import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { OperatorCredentialsCard } from './OperatorCredentialsCard'
import type { OperatorCredentialInventory } from '../lib/types'

const state = vi.hoisted(() => ({
  api: { getOperatorCredentials: vi.fn() },
  cluster: { id: 'local', baseURL: '/', apiKey: '', name: 'dev', version: '', capabilities: [] },
}))
vi.mock('../lib/cluster-context', () => ({ useCluster: () => state.cluster }))
vi.mock('../lib/api', () => ({ createApiClient: () => state.api }))
vi.mock('./SecurityHistoryCard', () => ({ SecurityHistoryCard: () => <div>Recent access</div> }))

const inventory: OperatorCredentialInventory = {
  observedAt: '2026-10-23T13:00:00Z', inventoryScope: 'current-control-plane', configurationAvailable: true,
  usageAvailable: false, warnings: [], credentials: [
    { id: 'admin', name: 'installation-admin', displayName: 'Installation administrator', accessLevel: 'administrator', state: 'accepted', source: 'installation', scopes: [], agentResources: ['*'], permissions: ['All operator actions on all Agents and Machines'], configured: true },
    { id: 'integration', name: 'jack-bridge', displayName: 'jack-bridge', accessLevel: 'restricted', state: 'accepted', source: 'configuration', scopes: ['tasks:create'], agentResources: ['kyber-system/jack'], permissions: ['Submit tasks'], configured: true },
    { id: 'inactive', name: 'configured-key', displayName: 'configured-key', accessLevel: 'restricted', state: 'configured-not-accepted', source: 'configuration', scopes: [], agentResources: [], permissions: [], configured: true },
  ],
}

afterEach(() => { cleanup(); vi.useRealTimers(); vi.clearAllMocks() })

describe('operator credential inventory', () => {
  it('shows registry ownership and keeps former grants out of active access', async () => {
    state.api.getOperatorCredentials.mockResolvedValue({
      ...inventory,
      inventoryScope: 'installation-registry',
      configurationAvailable: false,
      credentials: [
        { ...inventory.credentials[1], source: 'managed', state: 'revoked', displayName: 'Revoked integration', permissions: ['Previously could delete Agents'] },
        { ...inventory.credentials[1], id: 'expired', source: 'external', state: 'expired', displayName: 'Expired integration', permissions: ['Previously could read secrets'] },
      ],
    })
    render(<OperatorCredentialsCard />)
    expect(await screen.findByText('Revoked integration')).toBeInTheDocument()
    expect(screen.getByText('Revoked')).toBeInTheDocument()
    expect(screen.getByText('Expired')).toBeInTheDocument()
    expect(screen.getByText(/revoked and cannot authenticate/)).toBeInTheDocument()
    expect(screen.getByText(/expired and cannot authenticate/)).toBeInTheDocument()
    expect(screen.getByText(/Managed in Kyber/)).toBeInTheDocument()
    expect(screen.queryByText('Previously could delete Agents')).not.toBeInTheDocument()
    expect(screen.queryByText('Previously could read secrets')).not.toBeInTheDocument()
    expect(screen.queryByText(/Still active, but/)).not.toBeInTheDocument()
    expect(screen.getByText(/Changes to keys apply across this installation/)).toBeInTheDocument()
  })

  it('explains actual access and unavailable history without exposing key controls', async () => {
    state.api.getOperatorCredentials.mockResolvedValue(inventory)
    render(<OperatorCredentialsCard><button>Administrator rotation</button></OperatorCredentialsCard>)
    expect(await screen.findByText('Installation administrator')).toBeInTheDocument()
    expect(screen.getByText('jack-bridge')).toBeInTheDocument()
    expect(screen.getByText('Submit tasks')).toBeInTheDocument()
    expect(screen.getByText('Agent access: kyber-system/jack')).toBeInTheDocument()
    expect(screen.getByText('Configured, not active')).toBeInTheDocument()
    expect(screen.getByText(/Complete usage totals are not available/)).toBeInTheDocument()
    expect(screen.getByText('Recent access')).toBeInTheDocument()
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
    expect(screen.queryByText('Recent access')).not.toBeInTheDocument()
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
  it('keeps rotation controls mounted during a pending inventory refresh', async () => {
    state.api.getOperatorCredentials.mockResolvedValueOnce(inventory)
      .mockImplementationOnce(() => new Promise(() => {}))
    const user = userEvent.setup()
    render(<OperatorCredentialsCard><button>Administrator rotation</button></OperatorCredentialsCard>)
    const rotation = await screen.findByRole('button', { name: 'Administrator rotation' })
    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Loading credentials')
    expect(screen.getByRole('button', { name: 'Administrator rotation' })).toBe(rotation)
  })

  it('clears protected content when a periodic access check stalls', async () => {
    vi.useFakeTimers()
    state.api.getOperatorCredentials.mockResolvedValueOnce(inventory).mockImplementation((signal: AbortSignal) => new Promise((_, reject) => {
      signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })
    }))
    render(<OperatorCredentialsCard><button>Administrator rotation</button></OperatorCredentialsCard>)
    await act(async () => {})
    expect(screen.getByText('jack-bridge')).toBeInTheDocument()
    expect(screen.getByText('Recent access')).toBeInTheDocument()
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000) })
    await act(async () => { await vi.advanceTimersByTimeAsync(4_000) })
    expect(screen.getByRole('alert')).toHaveTextContent('Could not load operator credentials')
    expect(screen.queryByText('jack-bridge')).not.toBeInTheDocument()
    expect(screen.queryByText('Recent access')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Administrator rotation' })).not.toBeInTheDocument()
  })

  it.each(['machines:exec', 'machines:logs:read'])('shows Machine targets separately from Agent access (%s)', async (scope) => {
    state.api.getOperatorCredentials.mockResolvedValue({ ...inventory, credentials: [{
      ...inventory.credentials[1], scopes: [scope], agentResources: [],
      machineResources: ['kyber-system/worker-1'],
      permissions: ['Open host terminals and run commands on allowed Machines'],
    }] })
    render(<OperatorCredentialsCard />)
    expect(await screen.findByText('Machine access: kyber-system/worker-1')).toBeInTheDocument()
    expect(screen.getByText('Open host terminals and run commands on allowed Machines')).toBeInTheDocument()
    expect(screen.queryByText('Agent-specific permissions have no allowed target.')).not.toBeInTheDocument()
  })

})
