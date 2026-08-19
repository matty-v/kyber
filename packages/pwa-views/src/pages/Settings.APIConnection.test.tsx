import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { APIConnectionCard } from './Settings'

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('APIConnectionCard', () => {
  it('accepts typing while the key is masked', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false }))
    const user = userEvent.setup()

    render(<APIConnectionCard />)
    const input = screen.getByLabelText('API key')

    expect(input).toHaveAttribute('type', 'password')
    await user.type(input, 'test-api-key')
    expect(input).toHaveValue('test-api-key')
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
  })

  it.each([
    { ok: true, label: 'Set (browser session active)' },
    { ok: false, label: 'Not set' },
  ])('reports the browser-session status as $label', async ({ ok, label }) => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok }))

    render(<APIConnectionCard />)

    await waitFor(() => {
      expect(screen.getByText(label)).toBeInTheDocument()
    })
  })
})
