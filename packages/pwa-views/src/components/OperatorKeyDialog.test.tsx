import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { OperatorKeyDialog } from './OperatorKeyDialog'
import type { ApiClient } from '../lib/api'
import type { OperatorCredentialInventory, OperatorCredentialOptions } from '../lib/types'

afterEach(cleanup)
const inventory: OperatorCredentialInventory = { version: 'reviewed-version', currentCredentialId: 'admin', managementAvailable: true, observedAt: '2026-10-24T00:00:00Z', inventoryScope: 'installation-registry', configurationAvailable: false, usageAvailable: false, warnings: [], credentials: [] }
const options: OperatorCredentialOptions = { version: 1, namespace: 'test', permissions: [{ scope: 'metrics:read', label: 'Read fleet metrics', target: 'none' }] }
function setup() {
  const api = { mutateOperatorCredential: vi.fn(), getOperatorCredentialOperation: vi.fn(), reconcileOperatorCredentialOperation: vi.fn() }
  const onClose = vi.fn(), onCompleted = vi.fn()
  const props = { action: { kind: 'create' as const }, api: api as unknown as ApiClient, inventory, options, agents: [], machines: [], targetsUnavailable: false, bearerConnection: false, onClose, onCompleted }
  return { api, onClose, onCompleted, props, user: userEvent.setup() }
}
async function prepare(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('Name'), 'Metrics reader')
  await user.click(screen.getByLabelText('Read fleet metrics'))
  await user.click(screen.getByRole('button', { name: 'Next' }))
  expect(screen.getByRole('button', { name: 'Activate key' })).toBeDisabled()
  await user.click(screen.getByLabelText('I’ve saved this key value.'))
}
describe('operator access key form', () => {
  it('closes without submitting a valid policy', async () => {
    const { props, user, api, onClose } = setup(); render(<OperatorKeyDialog {...props} />)
    await user.type(screen.getByLabelText('Name'), 'Reader')
    await user.click(screen.getByLabelText('Read fleet metrics'))
    await user.click(screen.getByRole('button', { name: 'Close' }))
    expect(onClose).toHaveBeenCalledOnce(); expect(api.mutateOperatorCredential).not.toHaveBeenCalled()
    expect(screen.queryByText('New key value')).not.toBeInTheDocument()
  })
  it('requires saving the value, submits once, and checks an interrupted operation without replay', async () => {
    const { props, user, api } = setup(); api.mutateOperatorCredential.mockRejectedValue(new Error('offline'))
    api.getOperatorCredentialOperation.mockResolvedValue({ status: 'pending' })
    render(<OperatorKeyDialog {...props} />); await prepare(user)
    const value = (screen.getByLabelText('New key value') as HTMLInputElement).value
    expect(value).toMatch(/^[A-Za-z0-9_-]{43}$/)
    await user.click(screen.getByRole('button', { name: 'Activate key' }))
    expect(await screen.findByText('The key change is not confirmed yet.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Check status' }))
    expect(api.mutateOperatorCredential).toHaveBeenCalledOnce()
    const [request, version] = api.mutateOperatorCredential.mock.calls[0]
    expect(version).toBe('reviewed-version'); expect(request.key).toBe(value)
    expect(api.getOperatorCredentialOperation).toHaveBeenCalledWith(request.operationId, expect.any(AbortSignal))
    expect(api.reconcileOperatorCredentialOperation).not.toHaveBeenCalled()
    expect(window.localStorage.length).toBe(0); expect(window.sessionStorage.length).toBe(0)
  })
  it('keeps the review version fixed when the inventory refreshes', async () => {
    const { props, user, api } = setup(); api.mutateOperatorCredential.mockResolvedValue({ status: 'success' })
    const view = render(<OperatorKeyDialog {...props} />); await prepare(user)
    view.rerender(<OperatorKeyDialog {...props} inventory={{ ...inventory, version: 'new-version' }} />)
    await user.click(screen.getByRole('button', { name: 'Activate key' }))
    expect(api.mutateOperatorCredential.mock.calls[0][1]).toBe('reviewed-version')
  })
})

describe('revoking access keys', () => {
  it('requires confirmation and surfaces last-administrator protection without retry', async () => {
    const { props, api, user } = setup()
    api.mutateOperatorCredential.mockRejectedValue(Object.assign(new Error('Keep an administrator key.'), { code: 'last_administrator_required' }))
    const credential = { id: 'admin', name: 'Admin', displayName: 'Admin', accessLevel: 'administrator' as const, state: 'accepted' as const, source: 'installation' as const, scopes: [], agentResources: ['*'], permissions: [], configured: false }
    render(<OperatorKeyDialog {...props} action={{ kind: 'revoke', credential }} />)
    expect(api.mutateOperatorCredential).not.toHaveBeenCalled()
    expect(screen.getByText(/This includes your current connection/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Revoke key' }))
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Revoke key' })).toBeDisabled()
    expect(api.mutateOperatorCredential).toHaveBeenCalledOnce()
    expect(api.mutateOperatorCredential.mock.calls[0][0]).toEqual({ operationId: expect.any(String), action: 'revoke', credentialId: 'admin' })
  })
})
