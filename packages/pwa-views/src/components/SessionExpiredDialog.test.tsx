import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SessionExpiredDialog } from './SessionExpiredDialog'
import { ERR_SESSION_EXPIRED, createApiClient } from '../lib/api'
import type { Cluster } from '../lib/types'

// The dialog listens for the control plane's session_expired code, which
// createApiClient's request() emits on a 401. These tests drive it through
// that real path rather than calling the notifier directly, so a change that
// stops firing the signal fails here instead of passing quietly.

const cluster: Cluster = {
  id: 'local',
  name: 'test',
  baseURL: 'http://cp.test/',
  // Empty apiKey = embedded mode: auth rides on the HttpOnly cookie, which
  // is the only mode where a session can expire.
  apiKey: '',
}

function mockFetchOnce(status: number, body: unknown) {
  return vi.fn().mockResolvedValue({
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  })
}

beforeEach(() => {
  vi.stubGlobal('fetch', mockFetchOnce(200, {}))
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('SessionExpiredDialog', () => {
  it('stays closed until a session actually expires', () => {
    render(<SessionExpiredDialog />)
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('opens when an API call reports session_expired', async () => {
    render(<SessionExpiredDialog />)
    vi.stubGlobal(
      'fetch',
      mockFetchOnce(401, { error: { code: ERR_SESSION_EXPIRED, message: 'expired' } }),
    )

    const api = createApiClient(cluster)
    await expect(api.listAgents()).rejects.toThrow()

    await waitFor(() => {
      expect(screen.getByRole('dialog')).toBeInTheDocument()
    })
    expect(screen.getByText('Session expired')).toBeInTheDocument()
    expect(screen.getByLabelText('API Key')).toBeInTheDocument()
  })

  // A wrong API key is NOT a recoverable session — re-prompting for a
  // session there would loop the operator through a dialog that cannot
  // help. Only the dedicated code opens it.
  it('ignores a generic 401', async () => {
    render(<SessionExpiredDialog />)
    vi.stubGlobal(
      'fetch',
      mockFetchOnce(401, { error: { code: 'unauthorized', message: 'invalid API key' } }),
    )

    const api = createApiClient(cluster)
    await expect(api.listAgents()).rejects.toThrow()

    // Give any async open a chance to land before asserting it did not.
    await new Promise((r) => setTimeout(r, 0))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('ignores non-401 failures', async () => {
    render(<SessionExpiredDialog />)
    vi.stubGlobal(
      'fetch',
      mockFetchOnce(500, { error: { code: 'internal_error', message: 'boom' } }),
    )

    const api = createApiClient(cluster)
    await expect(api.listAgents()).rejects.toThrow()

    await new Promise((r) => setTimeout(r, 0))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('reports a rejected key instead of silently failing', async () => {
    render(<SessionExpiredDialog />)
    vi.stubGlobal(
      'fetch',
      mockFetchOnce(401, { error: { code: ERR_SESSION_EXPIRED, message: 'expired' } }),
    )
    const api = createApiClient(cluster)
    await expect(api.listAgents()).rejects.toThrow()
    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument())

    // The exchange endpoint now rejects the key the operator typed.
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false, status: 401 }))

    await userEvent.type(screen.getByLabelText('API Key'), 'wrong-key')
    await userEvent.click(screen.getByRole('button', { name: /reconnect/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/rejected/i)
    })
    // Still open — the operator gets another try rather than being dropped
    // back onto the broken page.
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('cannot be submitted with an empty key', async () => {
    render(<SessionExpiredDialog />)
    vi.stubGlobal(
      'fetch',
      mockFetchOnce(401, { error: { code: ERR_SESSION_EXPIRED, message: 'expired' } }),
    )
    const api = createApiClient(cluster)
    await expect(api.listAgents()).rejects.toThrow()
    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument())

    expect(screen.getByRole('button', { name: /reconnect/i })).toBeDisabled()
  })
})
