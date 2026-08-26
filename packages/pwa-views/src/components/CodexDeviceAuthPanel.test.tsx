import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import type { CodexDeviceAuthStatus } from '../lib/types'

const mutate = vi.fn()
let status: { data?: CodexDeviceAuthStatus; isLoading: boolean } = { isLoading: false }

vi.mock('../hooks/useAPI', () => ({
  useStartCodexDeviceAuth: () => ({ mutate, isPending: false }),
  useCodexDeviceAuthStatus: (_name: string, enabled: boolean) => {
    lastEnabled = enabled
    return status
  },
}))

let lastEnabled = false

const { CodexDeviceAuthPanel } = await import('./CodexDeviceAuthPanel')

describe('CodexDeviceAuthPanel', () => {
  beforeEach(() => {
    mutate.mockClear()
    status = { isLoading: false }
    vi.useFakeTimers({ shouldAdvanceTime: true })
    vi.setSystemTime(new Date('2026-08-26T20:00:00Z'))
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  // The whole point of the redesign: the operator never has to read a terminal.
  it('shows the link as a real anchor and the code with a one-tap copy', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    const writeText = vi.fn().mockResolvedValue(undefined)
    // jsdom exposes navigator.clipboard as a getter-only property, so it has to
    // be redefined rather than assigned.
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })

    status = {
      isLoading: false,
      data: {
        state: 'ready',
        verificationUrl: 'https://auth.openai.com/codex/device',
        userCode: 'E7OV-KG840',
        expiresAt: '2026-08-26T20:15:00Z',
      },
    }
    render(<CodexDeviceAuthPanel name="codextest" phase="Starting" />)

    const link = screen.getByRole('link', { name: 'https://auth.openai.com/codex/device' })
    expect(link).toHaveAttribute('href', 'https://auth.openai.com/codex/device')
    expect(link).toHaveAttribute('target', '_blank')
    // Opening a third-party page from a token-bearing app: no window.opener.
    expect(link.getAttribute('rel')).toContain('noopener')

    expect(screen.getByText('E7OV-KG840')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /copy code/i }))
    expect(writeText).toHaveBeenCalledWith('E7OV-KG840')

    // 15 minutes out from the faked now.
    expect(screen.getByText(/expires in 15:00/i)).toBeInTheDocument()
  })

  it('spins while the flow is coming up instead of showing a dead panel', () => {
    status = { isLoading: false, data: { state: 'starting' } }
    render(<CodexDeviceAuthPanel name="codextest" phase="Starting" />)
    expect(screen.getByRole('status')).toHaveTextContent(/starting login/i)
    expect(screen.queryByRole('button', { name: /start device login/i })).not.toBeInTheDocument()
  })

  it('offers the button when nothing is running', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    status = { isLoading: false, data: { state: 'absent' } }
    render(<CodexDeviceAuthPanel name="codextest" phase="NeedsAuth" />)
    await user.click(screen.getByRole('button', { name: /start device login/i }))
    expect(mutate).toHaveBeenCalledWith('codextest')
  })

  // Deliberately not automatic — restarting on a timer burns one-time codes in
  // the background while nobody is looking.
  it('an expired code asks to start again rather than restarting itself', () => {
    status = { isLoading: false, data: { state: 'expired', userCode: 'E7OV-KG840' } }
    render(<CodexDeviceAuthPanel name="codextest" phase="NeedsAuth" />)
    expect(screen.getByText(/that code expired/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /start again/i })).toBeInTheDocument()
    expect(mutate).not.toHaveBeenCalled()
  })

  // The poll stops once a code is showing, so nothing would re-report the
  // expiry; the panel has to notice it from the deadline it already holds.
  it('treats a code whose deadline has passed as expired without another poll', () => {
    vi.setSystemTime(new Date('2026-08-26T20:16:00Z'))
    status = {
      isLoading: false,
      data: {
        state: 'ready',
        verificationUrl: 'https://auth.openai.com/codex/device',
        userCode: 'E7OV-KG840',
        expiresAt: '2026-08-26T20:15:00Z',
      },
    }
    render(<CodexDeviceAuthPanel name="codextest" phase="Starting" />)
    expect(screen.getByRole('button', { name: /start again/i })).toBeInTheDocument()
  })

  // No trustworthy deadline: show the code, drop the timer. A countdown from a
  // guessed origin is worse than none.
  it('omits the countdown when the server could not date the code', () => {
    status = {
      isLoading: false,
      data: {
        state: 'ready',
        verificationUrl: 'https://auth.openai.com/codex/device',
        userCode: 'E7OV-KG840',
      },
    }
    render(<CodexDeviceAuthPanel name="codextest" phase="Starting" />)
    expect(screen.getByText('E7OV-KG840')).toBeInTheDocument()
    expect(screen.queryByText(/expires in/i)).not.toBeInTheDocument()
  })

  // The POST behind this button wipes the Codex auth Secret back to {} and
  // restarts the agent. `absent` during a boot means "the pod has not reached
  // the login step yet", not "nothing is coming" — offering the button there
  // invites a restart of the boot that is about to print the code.
  it('waits rather than offering a restart while a Starting pod has no session yet', () => {
    status = { isLoading: false, data: { state: 'absent' } }
    render(<CodexDeviceAuthPanel name="codextest" phase="Starting" />)
    expect(screen.getByRole('status')).toHaveTextContent(/starting login/i)
    expect(screen.queryByRole('button', { name: /start device login/i })).not.toBeInTheDocument()
  })

  // Each poll is an exec into the pod. Polling an agent that is not mid-login
  // would be a steady stream of them across the fleet.
  it('only polls while a login could be running', () => {
    status = { isLoading: false, data: { state: 'absent' } }
    render(<CodexDeviceAuthPanel name="codextest" phase="Running" />)
    expect(lastEnabled).toBe(false)
  })
})
