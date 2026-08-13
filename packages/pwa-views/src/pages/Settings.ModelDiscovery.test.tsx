import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'

vi.mock('../hooks/useAPI', () => ({
  useAvailable: vi.fn(() => ({ data: undefined })),
  useAnthropicKeyStatus: vi.fn(),
  useSetAnthropicKey: vi.fn(() => ({ mutateAsync: vi.fn(), isPending: false, isSuccess: false })),
  useClearAnthropicKey: vi.fn(() => ({ mutateAsync: vi.fn(), isPending: false })),
}))

import * as useAPIModule from '../hooks/useAPI'
import { ModelDiscoveryCard } from './Settings'

function mockStatus(ret: unknown) {
  vi.mocked(useAPIModule.useAnthropicKeyStatus).mockReturnValue(
    ret as ReturnType<typeof useAPIModule.useAnthropicKeyStatus>,
  )
}

describe('ModelDiscoveryCard — Anthropic', () => {
  beforeEach(() => vi.clearAllMocks())

  // The bug: an install with runtimeDetect disabled has no Secret to write
  // into, so the control plane 503s — but the card still rendered the input and
  // Save button, and the operator found out only after typing a live
  // credential into it.
  it('offers no key field when the control plane cannot store one', () => {
    mockStatus({ data: undefined, error: { status: 503 } })
    render(<ModelDiscoveryCard />)

    expect(screen.queryByLabelText('Anthropic API key')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
    expect(screen.getByText(/Model discovery is turned off/)).toBeInTheDocument()
    // Name the thing that turns it back on, so the message is actionable.
    expect(screen.getByText(/runtimeDetect.enabled/)).toBeInTheDocument()
  })

  it('offers the key field when the control plane can store one', () => {
    mockStatus({ data: { configured: false }, error: null })
    render(<ModelDiscoveryCard />)

    expect(screen.getByLabelText('Anthropic API key')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save' })).toBeInTheDocument()
    expect(screen.queryByText(/Model discovery is turned off/)).not.toBeInTheDocument()
  })

  it('shows Replace and Clear once a key is configured', () => {
    mockStatus({ data: { configured: true }, error: null })
    render(<ModelDiscoveryCard />)

    expect(screen.getByRole('button', { name: 'Replace' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Clear' })).toBeInTheDocument()
  })

  // A transport blip is not the same as "this install cannot do this", and it
  // must not silently hide the control.
  it('still offers the key field when the failure is not a 503', () => {
    mockStatus({ data: undefined, error: { status: 500 } })
    render(<ModelDiscoveryCard />)

    expect(screen.getByLabelText('Anthropic API key')).toBeInTheDocument()
  })
})
