import { act, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ApiClient } from '../lib/api'
import { OperatorCredentialMaintenance } from './OperatorCredentialMaintenance'
const blocked = { observedAt: '2026-10-24T12:00:00Z', blocked: true, version: `"${'A'.repeat(43)}"`, operation: { kind: 'upgrade', executionId: 'e703854e-4d01-45d7-a321-5c737d173d46', startedAt: '2000-01-01T00:00:00Z' } }
function client(load: () => Promise<unknown>) { return { getOperatorCredentialMaintenance: vi.fn(load) } as unknown as ApiClient }
describe('credential maintenance visibility', () => {
 it('keeps an old operation blocked and exposes no release action', async () => {
  const api = client(async () => blocked)
  render(<OperatorCredentialMaintenance api={api} enabled />)
  expect(await screen.findByText('An upgrade operation holds credential maintenance.')).toBeVisible()
  expect(screen.getByText(/does not expire automatically/)).toBeVisible()
  expect(screen.queryByRole('button')).not.toBeInTheDocument()
 })
 it('does not turn malformed or failed reads into idle', async () => {
  const api = client(async () => ({ ...blocked, operation: undefined }))
  const view = render(<OperatorCredentialMaintenance api={api} enabled />)
  expect(await screen.findByRole('alert')).toHaveTextContent('unavailable')
  expect(screen.queryByText('No credential maintenance block.')).not.toBeInTheDocument()
  const failure = client(async () => { throw { status: 403 } })
  view.rerender(<OperatorCredentialMaintenance api={failure} enabled />)
  expect(await screen.findByRole('alert')).toHaveTextContent('unavailable')
 })
 it('suppresses the previous connection while another request is pending', async () => {
  const api = client(async () => blocked)
  const view = render(<OperatorCredentialMaintenance api={api} enabled />)
  await screen.findByText('An upgrade operation holds credential maintenance.')
  let finish!: (value: unknown) => void
  const second = client(() => new Promise(resolve => { finish = resolve }))
  view.rerender(<OperatorCredentialMaintenance api={second} enabled />)
  expect(screen.queryByText('An upgrade operation holds credential maintenance.')).not.toBeInTheDocument()
  await act(async () => { finish({ observedAt: blocked.observedAt, blocked: false }) })
  expect(await screen.findByText('No credential maintenance block.')).toBeVisible()
 })
 it('does not read unsupported installations', async () => {
  const api = client(async () => blocked)
  const view = render(<OperatorCredentialMaintenance api={api} enabled={false} />)
  await waitFor(() => expect(api.getOperatorCredentialMaintenance).not.toHaveBeenCalled())
  expect(view.container).toBeEmptyDOMElement()
 })
})
