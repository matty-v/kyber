import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ApiClient } from '../lib/api'
import { OperatorCredentialAdoption } from './OperatorCredentialAdoption'

afterEach(cleanup)
const id = 'a34e17a4-31ad-4ea4-90de-6d635e47c335'
const outcome = { operationId: id, status: 'pending', recoverable: false, observedAt: '2026-10-25T20:00:00Z' }
function client() {
  return {
    prepareOperatorCredentialAdoption: vi.fn().mockResolvedValue({ version: 'a'.repeat(64), keyCount: 3, reconnectCount: 0 }),
    startOperatorCredentialAdoption: vi.fn().mockImplementation(async (input: {operationId: string}) => ({ ...outcome, operationId: input.operationId })),
    getPendingOperatorCredentialAdoptions: vi.fn().mockResolvedValue({ items: [], limit: 25 }),
    getOperatorCredentialAdoption: vi.fn().mockResolvedValue(outcome),
    recoverOperatorCredentialAdoption: vi.fn().mockResolvedValue(outcome),
  }
}
function content(api: ReturnType<typeof client>, allowStart = true) {
  return <OperatorCredentialAdoption api={api as unknown as ApiClient} allowStart={allowStart} onCompleted={vi.fn()} />
}
it('requires explicit ownership confirmation and never launches on mount or status reads', async () => {
  const api = client(), user = userEvent.setup(); render(content(api))
  expect(api.prepareOperatorCredentialAdoption).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', {name: 'Check key setup'}))
  expect(api.startOperatorCredentialAdoption).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', {name: 'Manage existing keys in Kyber'}))
  expect(await screen.findByText('Keep 3 existing keys and their access.')).toBeInTheDocument()
  expect(screen.getByRole('button', {name: 'Confirm setup'})).toBeDisabled()
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', {name: 'Confirm setup'}))
  await screen.findByText('Key setup is pending.')
  expect(api.startOperatorCredentialAdoption).toHaveBeenCalledTimes(1)
  expect(api.startOperatorCredentialAdoption.mock.calls[0][0]).toMatchObject({version: 'a'.repeat(64), confirmOwnership: true})
})
it('retains the original operation after an uncertain write and checks it without resending', async () => {
  const api = client(), user = userEvent.setup(); api.startOperatorCredentialAdoption.mockRejectedValue(new Error('lost response'))
  render(content(api))
  await user.click(screen.getByRole('button', {name: 'Manage existing keys in Kyber'}))
  await user.click(await screen.findByRole('checkbox'))
  await user.click(screen.getByRole('button', {name: 'Confirm setup'}))
  await screen.findByText('Setup outcome is unconfirmed.')
  const original = api.startOperatorCredentialAdoption.mock.calls[0][0].operationId
  api.getOperatorCredentialAdoption.mockResolvedValue({...outcome, operationId: original})
  expect(screen.getByRole('button', {name: 'Manage existing keys in Kyber'})).toBeDisabled()
  await user.click(screen.getByRole('button', {name: 'Check outcome'}))
  await screen.findByText('Key setup is pending.')
  expect(api.getOperatorCredentialAdoption.mock.calls[0][0]).toBe(original)
  expect(api.startOperatorCredentialAdoption).toHaveBeenCalledTimes(1)
})
it('clears protected setup state immediately when the connection changes', async () => {
  const first = client(), second = client(), user = userEvent.setup(); const view = render(content(first))
  await user.click(screen.getByRole('button', {name: 'Manage existing keys in Kyber'}))
  await screen.findByRole('checkbox')
  view.rerender(content(second))
  expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  expect(second.startOperatorCredentialAdoption).not.toHaveBeenCalled()
})
it('removes operation details and all write controls after access is denied', async () => {
  const api = client(), user = userEvent.setup(); api.getPendingOperatorCredentialAdoptions.mockRejectedValue({status: 403})
  render(content(api)); await user.click(screen.getByRole('button', {name: 'Check key setup'}))
  await screen.findByText('Administrator access is no longer available.')
  expect(screen.getByRole('button', {name: 'Manage existing keys in Kyber'})).toBeDisabled()
  expect(screen.queryByText(/Operation details/)).not.toBeInTheDocument()
})
it('offers recovery only after a fresh server observation confirms it is eligible', async () => {
  const api = client(), user = userEvent.setup()
  api.getPendingOperatorCredentialAdoptions.mockResolvedValue({items: [{...outcome, recoverable: true}], limit: 25})
  render(content(api, false)); expect(screen.queryByRole('button', {name: 'Continue setup'})).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', {name: 'Check key setup'}))
  await user.click(await screen.findByRole('button', {name: 'Continue setup'}))
  await vi.waitFor(() => expect(api.recoverOperatorCredentialAdoption).toHaveBeenCalledTimes(1))
  expect(api.recoverOperatorCredentialAdoption.mock.calls[0][0]).toBe(id)
  expect(api.startOperatorCredentialAdoption).not.toHaveBeenCalled()
})
