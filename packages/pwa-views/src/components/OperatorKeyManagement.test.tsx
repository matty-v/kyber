import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { OperatorKeyManagement } from './OperatorKeyManagement'
import type { ApiClient } from '../lib/api'
import type { OperatorCredentialInventory } from '../lib/types'

afterEach(cleanup)
const inventory: OperatorCredentialInventory = { version: 'opening-version', managementAvailable: true, currentCredentialId: 'admin', inventoryScope: 'installation-registry', observedAt: '2026-10-24T00:00:00Z', configurationAvailable: false, usageAvailable: false, warnings: [], credentials: [] }
function client() {
  return {
    getOperatorCredentialOptions: vi.fn().mockResolvedValue({ version: 1, namespace: 'test', permissions: [{ scope: 'metrics:read', label: 'Read metrics', target: 'none' }] }),
    listAgents: vi.fn().mockResolvedValue([]), listMachines: vi.fn().mockResolvedValue([]),
    getPendingOperatorCredentialOperations: vi.fn().mockResolvedValue({ items: [] }),
    mutateOperatorCredential: vi.fn().mockResolvedValue({ status: 'success' }),
  }
}
function content(api: ReturnType<typeof client>, data = inventory) {
  return <OperatorKeyManagement api={api as unknown as ApiClient} inventory={data} bearerConnection={false} onCompleted={() => {}}>{() => null}</OperatorKeyManagement>
}
it('discards prepared key forms immediately when the connection changes', async () => {
  const first = client(), second = client(), user = userEvent.setup()
  const view = render(content(first))
  await vi.waitFor(() => expect(screen.getByRole('button', { name: 'New access key' })).toBeEnabled())
  await user.click(screen.getByRole('button', { name: 'New access key' }))
  await user.type(screen.getByLabelText('Name'), 'Reader')
  await user.click(screen.getByLabelText('Read metrics'))
  await user.click(screen.getByRole('button', { name: 'Next' }))
  expect(screen.getByLabelText('New key value')).toBeInTheDocument()
  view.rerender(content(second))
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  expect(screen.queryByLabelText('New key value')).not.toBeInTheDocument()
  expect(first.mutateOperatorCredential).not.toHaveBeenCalled()
  expect(second.mutateOperatorCredential).not.toHaveBeenCalled()
})
it('retries failed choices without requiring a connection change', async () => {
  const api = client(), user = userEvent.setup()
  api.getOperatorCredentialOptions.mockRejectedValueOnce(new Error('unavailable'))
  render(content(api))
  await user.click(await screen.findByRole('button', { name: 'Retry key choices' }))
  await vi.waitFor(() => expect(screen.getByRole('button', { name: 'New access key' })).toBeEnabled())
  expect(api.getOperatorCredentialOptions).toHaveBeenCalledTimes(2)
})
