import { afterEach, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { OperatorKeyUsage } from './OperatorKeyUsage'
import type { ApiClient } from '../lib/api'
import { usageCredential, usageView } from '../test/operator-usage-fixture'

afterEach(cleanup)
function content(api: { getOperatorCredentialUsage: ReturnType<typeof vi.fn> }, revision = 0) {
  return <OperatorKeyUsage api={api as unknown as ApiClient} enabled revision={revision}>{(usage) => usage(usageCredential)}</OperatorKeyUsage>
}
it('shows exact observed counts and their partial observation window', async () => {
  const api = { getOperatorCredentialUsage: vi.fn().mockResolvedValue(usageView()) }
  render(content(api))
  expect(await screen.findByText(/authenticated requests/)).toHaveTextContent('9,007,199,254,740,993')
  expect(screen.getByText(/Last observed use/)).toBeInTheDocument()
  expect(screen.getByText(/Partial coverage/)).toBeInTheDocument()
  expect(screen.queryByText(/never used/i)).not.toBeInTheDocument()
})
it('shows no recorded use only when an observation window exists', async () => {
  const data = usageView(); data.items = []
  const api = { getOperatorCredentialUsage: vi.fn().mockResolvedValue(data) }
  const view = render(content(api))
  expect(await screen.findByText('No accepted use recorded in this window.')).toBeInTheDocument()
  const starting = usageView(); starting.items = []; delete starting.coverage.since
  starting.coverage.currentObserverReported = false; starting.coverage.currentObserverFresh = false
  api.getOperatorCredentialUsage.mockResolvedValue(starting)
  view.rerender(content(api, 1))
  expect(await screen.findByText('Activity collection is starting.')).toBeInTheDocument()
  expect(screen.queryByText('No accepted use recorded in this window.')).not.toBeInTheDocument()
})
it('clears old counts on reporting failure and reports a gap instead of a zero', async () => {
  const api = { getOperatorCredentialUsage: vi.fn().mockResolvedValue(usageView()) }
  const view = render(content(api)); await screen.findByText(/authenticated requests/)
  api.getOperatorCredentialUsage.mockRejectedValue(Object.assign(new Error('unavailable'), { status: 503 }))
  view.rerender(content(api, 1))
  expect(await screen.findByRole('alert')).toHaveTextContent('Key activity is unavailable')
  expect(screen.queryAllByText('9,007,199,254,740,993')).toHaveLength(0)
  expect(screen.queryByText('No accepted use recorded in this window.')).not.toBeInTheDocument()
})
it('never paints another connection’s counters even when credential identities match', async () => {
  const first = { getOperatorCredentialUsage: vi.fn().mockResolvedValue(usageView()) }
  let resolve: (data: ReturnType<typeof usageView>) => void = () => {}
  const second = { getOperatorCredentialUsage: vi.fn().mockImplementation(() => new Promise((done) => { resolve = done })) }
  const view = render(content(first)); await screen.findByText(/authenticated requests/)
  view.rerender(content(second))
  expect(screen.queryAllByText('9,007,199,254,740,993')).toHaveLength(0)
  const next = usageView(); next.items[0].authenticated = '7'; next.items[0].actions = { 'agents:read': '7' }
  await act(async () => { resolve(next) })
  await waitFor(() => expect(screen.getByText(/authenticated requests/)).toHaveTextContent('7 authenticated requests'))
  expect(screen.queryAllByText('9,007,199,254,740,993')).toHaveLength(0)
})
it('clears protected activity on denial and marks recorded or unconfirmed losses', async () => {
  const data = usageView(); data.coverage.dropped = '3'; data.local!.lossReportsAwaitingConfirmation = '1'
  const api = { getOperatorCredentialUsage: vi.fn().mockResolvedValue(data) }
  const view = render(content(api))
  expect(await screen.findByText('Some activity observations were lost.')).toBeInTheDocument()
  api.getOperatorCredentialUsage.mockRejectedValue(Object.assign(new Error('forbidden'), { status: 403 }))
  view.rerender(content(api, 1))
  expect(await screen.findByText('Administrator access is required to view key activity.')).toBeInTheDocument()
  expect(screen.queryAllByText('9,007,199,254,740,993')).toHaveLength(0)
})
