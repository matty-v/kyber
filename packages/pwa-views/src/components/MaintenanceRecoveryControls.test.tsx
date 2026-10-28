import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ApiClient } from '../lib/api'
import { MaintenanceRecoveryControls } from './MaintenanceRecoveryControls'
const version = `"${'A'.repeat(43)}"`
const operationId = '57e9038a-c35e-4b7b-bcb7-afcd532c28a1'
const pending = { operationId, status: 'pending', observedAt: '2026-10-24T12:00:00Z' }
function fixture() {
 return {
  checkMaintenanceRecovery: vi.fn(async () => ({ ready: true, reason: 'ready', version })),
  recoverCredentialMaintenance: vi.fn(async (..._args: unknown[]) => pending),
  getMaintenanceRecoveryOperation: vi.fn(async (..._args: unknown[]) => pending),
  getPendingMaintenanceRecoveryOperations: vi.fn(async () => ({ items: [pending] })),
  resolveMaintenanceRecovery: vi.fn(async (..._args: unknown[]) => ({ ...pending, status: 'success' })),
 }
}
describe('maintenance recovery controls', () => {
 it('does not read or mutate until requested and requires a matching readiness check', async () => {
  const api = fixture()
  render(<MaintenanceRecoveryControls api={api as unknown as ApiClient} version={version} />)
  expect(api.checkMaintenanceRecovery).not.toHaveBeenCalled()
  expect(screen.queryByText('Recover maintenance')).not.toBeInTheDocument()
  fireEvent.click(screen.getByText('Check recovery'))
  fireEvent.click(await screen.findByText('Recover maintenance'))
  await waitFor(() => expect(api.recoverCredentialMaintenance).toHaveBeenCalledTimes(1))
  expect(api.recoverCredentialMaintenance.mock.calls[0]?.[1]).toBe(version)
  expect(await screen.findByText('Recovery pending.')).toBeVisible()
  expect(api.resolveMaintenanceRecovery).not.toHaveBeenCalled()
 })
 it('keeps checking an outcome read-only and resolves only on explicit request', async () => {
  const api = fixture()
  render(<MaintenanceRecoveryControls api={api as unknown as ApiClient} />)
  fireEvent.click(screen.getByText('Check pending recoveries'))
  fireEvent.click(await screen.findByText('Check outcome'))
  await waitFor(() => expect(api.getMaintenanceRecoveryOperation).toHaveBeenCalledTimes(1))
  expect(api.resolveMaintenanceRecovery).not.toHaveBeenCalled()
  fireEvent.click(screen.getByText('Resolve recovery'))
  expect(await screen.findByText('Maintenance recovered.')).toBeVisible()
  expect(api.resolveMaintenanceRecovery).toHaveBeenCalledTimes(1)
 })
 it('never enables recovery for a stale version', async () => {
  const api = fixture()
  api.checkMaintenanceRecovery.mockResolvedValue({ ready: true, reason: 'ready', version: 'stale' })
  render(<MaintenanceRecoveryControls api={api as unknown as ApiClient} version={version} />)
  fireEvent.click(screen.getByText('Check recovery'))
  expect(await screen.findByRole('alert')).toBeVisible()
  expect(screen.queryByText('Recover maintenance')).not.toBeInTheDocument()
  expect(api.recoverCredentialMaintenance).not.toHaveBeenCalled()
 })
 it('clears protected metadata and disables controls when administrator access is lost', async () => {
  const api = fixture()
  api.checkMaintenanceRecovery.mockRejectedValue({ status: 403 })
  render(<MaintenanceRecoveryControls api={api as unknown as ApiClient} version={version} />)
  fireEvent.click(screen.getByText('Check pending recoveries'))
  await screen.findByText('Recovery pending.')
  fireEvent.click(screen.getByText('Check recovery'))
  expect(await screen.findByRole('alert')).toHaveTextContent('Administrator access is no longer available.')
  expect(screen.queryByText('Recovery pending.')).not.toBeInTheDocument()
  expect(screen.getByText('Check pending recoveries')).toBeDisabled()
 })
 it('discards a pending result after unmount', async () => {
  const api = fixture()
  let finish!: (value: { ready: boolean; reason: string; version: string }) => void
  api.checkMaintenanceRecovery.mockImplementation(() => new Promise(resolve => { finish = resolve }))
  const view = render(<MaintenanceRecoveryControls api={api as unknown as ApiClient} version={version} />)
  fireEvent.click(screen.getByText('Check recovery'))
  view.unmount()
  await act(async () => { finish({ ready: true, reason: 'ready', version }) })
  expect(api.recoverCredentialMaintenance).not.toHaveBeenCalled()
 })
 it('retains the original operation ID after an unconfirmed write without retrying it', async () => {
  const api = fixture()
  api.recoverCredentialMaintenance.mockRejectedValue(new Error('lost acknowledgement'))
  render(<MaintenanceRecoveryControls api={api as unknown as ApiClient} version={version} />)
  fireEvent.click(screen.getByText('Check recovery'))
  fireEvent.click(await screen.findByText('Recover maintenance'))
  expect(await screen.findByText('Recovery outcome is unconfirmed.')).toBeVisible()
  const submitted = api.recoverCredentialMaintenance.mock.calls[0]?.[0]
  fireEvent.click(screen.getByText('Check outcome'))
  await waitFor(() => expect(api.getMaintenanceRecoveryOperation).toHaveBeenCalledTimes(1))
  expect(api.getMaintenanceRecoveryOperation.mock.calls[0]?.[0]).toBe(submitted)
  expect(api.recoverCredentialMaintenance).toHaveBeenCalledTimes(1)
  expect(api.resolveMaintenanceRecovery).not.toHaveBeenCalled()
 })
 it('admits only one write when clicked twice before the response', async () => {
  const api = fixture()
  let finish!: (value: typeof pending) => void
  api.recoverCredentialMaintenance.mockImplementation(() => new Promise(resolve => { finish = resolve }))
  render(<MaintenanceRecoveryControls api={api as unknown as ApiClient} version={version} />)
  fireEvent.click(screen.getByText('Check recovery'))
  const button = await screen.findByText('Recover maintenance')
  fireEvent.click(button); fireEvent.click(button)
  expect(api.recoverCredentialMaintenance).toHaveBeenCalledTimes(1)
  await act(async () => { finish(pending) })
  expect(await screen.findByText('Recovery pending.')).toBeVisible()
 })

})
