import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SecurityHistoryCard } from './SecurityHistoryCard'
import type { OperatorCredential, SecurityEvent, SecurityEventPage } from '../lib/types'

const state = vi.hoisted(() => ({
  api: { getSecurityEvents: vi.fn() },
  cluster: { id: 'local', baseURL: '/', apiKey: '', name: 'dev', version: '', capabilities: [] },
}))
vi.mock('../lib/cluster-context', () => ({ useCluster: () => state.cluster }))
vi.mock('../lib/api', () => ({ createApiClient: () => state.api }))

const credentials: OperatorCredential[] = [{
  id: 'administrator', name: 'installation-admin', displayName: 'Installation administrator',
  accessLevel: 'administrator', state: 'accepted', source: 'installation', scopes: [], agentResources: ['*'], permissions: [], configured: true,
}]
const event: SecurityEvent = {
  id: 'record-one', operationId: 'operation-one', observedAt: '2026-10-23T13:00:00Z', storedAt: '2026-10-23T13:00:00Z',
  sequence: '9007199254740993', namespace: 'kyber-system', principalId: 'admin-principal', credentialId: 'administrator',
  credentialGeneration: '18446744073709551615', credentialAuthority: 'configuration', requestId: 'request-one',
  method: 'GET', action: 'secrets:read', resourceKind: 'agent', resourceName: 'alpha', phase: 'authorization', outcome: 'allow',
}
const page = (items: SecurityEvent[] = [event]): SecurityEventPage => ({
  observedAt: event.observedAt, items, snapshot: '9007199254740993', coverage: 'partial-sensitive-access', warnings: ['Selected sensitive access only.'],
})

beforeEach(() => { state.api = { getSecurityEvents: vi.fn() }; state.cluster = { ...state.cluster, id: 'local', apiKey: '' } })
afterEach(() => { cleanup(); vi.clearAllMocks() })

describe('security history', () => {
  it('distinguishes allowed access from completed work and states coverage', async () => {
    state.api.getSecurityEvents.mockResolvedValue(page([event, { ...event, id: 'record-two', sequence: '9007199254740994', phase: 'completion', outcome: 'success' }]))
    render(<SecurityHistoryCard credentials={credentials} />)
    expect(await screen.findByText('Access allowed')).toBeInTheDocument()
    expect(screen.getByText('Request completed')).toBeInTheDocument()
    expect(screen.getAllByText('Read Agent credential values')).toHaveLength(2)
    expect(screen.getByText(/No recorded activity does not mean/)).toBeInTheDocument()
    expect(screen.getByText('Selected sensitive access only.')).toBeInTheDocument()
    expect(state.api.getSecurityEvents).toHaveBeenCalledWith({ limit: 25, credentialId: undefined, cursor: undefined, includeHistoryViews: false }, expect.any(AbortSignal))
  })

  it('keeps key filters on older pages and resets pagination on refresh', async () => {
    state.api.getSecurityEvents.mockResolvedValue({ ...page(), nextCursor: 'older-records' })
    const user = userEvent.setup()
    render(<SecurityHistoryCard credentials={credentials} />)
    await screen.findByText('Access allowed')
    await user.selectOptions(screen.getByRole('combobox', { name: 'Operator key' }), 'administrator')
    await waitFor(() => expect(state.api.getSecurityEvents).toHaveBeenLastCalledWith({ limit: 25, credentialId: 'administrator', cursor: undefined, includeHistoryViews: false }, expect.any(AbortSignal)))
    await user.click(await screen.findByRole('button', { name: 'Older activity' }))
    await waitFor(() => expect(state.api.getSecurityEvents).toHaveBeenLastCalledWith({ limit: 25, credentialId: 'administrator', cursor: 'older-records', includeHistoryViews: false }, expect.any(AbortSignal)))
    await user.click(await screen.findByRole('button', { name: 'Refresh activity' }))
    await waitFor(() => expect(state.api.getSecurityEvents).toHaveBeenLastCalledWith({ limit: 25, credentialId: 'administrator', cursor: undefined, includeHistoryViews: false }, expect.any(AbortSignal)))
  })

  it('can include history views without reusing the other selection cursor', async () => {
    state.api.getSecurityEvents.mockResolvedValue({ ...page(), nextCursor: 'older-records' })
    const user = userEvent.setup()
    render(<SecurityHistoryCard credentials={credentials} />)
    await screen.findByText('Access allowed')
    await user.click(screen.getByRole('button', { name: 'Older activity' }))
    await screen.findByText('Access allowed')
    await user.click(screen.getByRole('checkbox', { name: 'Include history views' }))
    await waitFor(() => expect(state.api.getSecurityEvents).toHaveBeenLastCalledWith({ limit: 25, credentialId: undefined, cursor: undefined, includeHistoryViews: true }, expect.any(AbortSignal)))
  })

  it.each([400, 403, 404, 503])('does not present an error as empty successful history (%s)', async (status) => {
    state.api.getSecurityEvents.mockRejectedValue({ status })
    render(<SecurityHistoryCard credentials={credentials} />)
    expect(await screen.findByRole('alert')).toHaveTextContent(status === 403 ? 'Administrator access' : status === 404 ? 'not available on this installation' : status === 400 ? 'history filters' : 'unavailable')
    expect(screen.queryByText('No recorded activity for this selection.')).not.toBeInTheDocument()
  })

  it('clears previously rendered records when access is denied', async () => {
    state.api.getSecurityEvents.mockResolvedValueOnce(page()).mockRejectedValue({ status: 403 })
    const user = userEvent.setup()
    render(<SecurityHistoryCard credentials={credentials} />)
    await screen.findByText('Agent alpha')
    await user.click(screen.getByRole('button', { name: 'Refresh activity' }))
    await screen.findByRole('alert')
    expect(screen.queryByText('Agent alpha')).not.toBeInTheDocument()
  })

  it('ignores late records from the previous installation and aborts their request', async () => {
    let resolveOld!: (data: SecurityEventPage) => void
    state.api.getSecurityEvents.mockImplementation(() => new Promise<SecurityEventPage>((resolve) => { resolveOld = resolve }))
    const oldAPI = state.api
    const view = render(<SecurityHistoryCard credentials={credentials} />)
    await waitFor(() => expect(oldAPI.getSecurityEvents).toHaveBeenCalled())
    const signal = oldAPI.getSecurityEvents.mock.calls[0][1] as AbortSignal
    state.api = { getSecurityEvents: vi.fn().mockResolvedValue(page([{ ...event, resourceName: 'beta' }])) }
    state.cluster = { ...state.cluster, id: 'other' }
    view.rerender(<SecurityHistoryCard credentials={credentials} />)
    expect(await screen.findByText('Agent beta')).toBeInTheDocument()
    resolveOld(page())
    await waitFor(() => expect(signal.aborted).toBe(true))
    expect(screen.queryByText('Agent alpha')).not.toBeInTheDocument()
  })

  it('does not assign an ambiguous credential ID to a single display name', async () => {
    state.api.getSecurityEvents.mockResolvedValue(page())
    render(<SecurityHistoryCard credentials={[...credentials, { ...credentials[0], displayName: 'Different consumer' }]} />)
    await screen.findByText('Access allowed')
    expect(screen.queryByText('Different consumer')).not.toBeInTheDocument()
    expect(screen.queryByText('Installation administrator')).not.toBeInTheDocument()
    expect(screen.getAllByText('administrator').length).toBeGreaterThan(0)
  })
})
