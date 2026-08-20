import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import type { UseQueryResult, UseMutationResult } from '@tanstack/react-query'
import type { ComputeConfig, CreateMachineRequest, Machine } from '../lib/types'

// Mock useAPI so we control provider + the mutation spy. Mirrors the pattern
// used by CreateAgent.test.tsx in this directory.
vi.mock('../hooks/useAPI', () => ({
  useComputeConfig: vi.fn(),
  useCreateMachine: vi.fn(),
}))

const navigateMock = vi.fn()
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom')
  return { ...actual, useNavigate: () => navigateMock }
})

import * as useAPIModule from '../hooks/useAPI'
import { CreateMachine } from './CreateMachine'

function setup(mutateAsync = vi.fn().mockResolvedValue({})) {
  vi.mocked(useAPIModule.useComputeConfig).mockReturnValue({
    data: { compute: { provider: 'mock' } } as ComputeConfig,
    isLoading: false,
    error: null,
  } as unknown as UseQueryResult<ComputeConfig, Error>)

  vi.mocked(useAPIModule.useCreateMachine).mockReturnValue({
    mutateAsync,
    isPending: false,
  } as unknown as UseMutationResult<Machine, Error, CreateMachineRequest, unknown>)

  return { mutateAsync }
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('CreateMachine (mock provider)', () => {
  it('renders only a Name field — no CPU / Memory / Disk pickers', () => {
    setup()
    render(
      <MemoryRouter>
        <CreateMachine />
      </MemoryRouter>,
    )
    // Name input is present.
    expect(screen.getByPlaceholderText('local')).toBeInTheDocument()
    // No CPU / Memory / Disk labels — these were the removed Selects.
    expect(screen.queryByText('CPU')).not.toBeInTheDocument()
    expect(screen.queryByText('Memory')).not.toBeInTheDocument()
    expect(screen.queryByText('Disk')).not.toBeInTheDocument()
    // Explainer copy mentions the carve-out so the operator knows why.
    expect(screen.getByText(/full capacity of this node/i)).toBeInTheDocument()
    expect(screen.getByText(/platformReservation/i)).toBeInTheDocument()
    expect(screen.getByText(/kyber\.io\/machine=/i)).toBeInTheDocument()
  })

  it('submits {name, provider:"mock"} with no capacity field', async () => {
    const user = userEvent.setup()
    const { mutateAsync } = setup()
    render(
      <MemoryRouter>
        <CreateMachine />
      </MemoryRouter>,
    )
    await user.type(screen.getByPlaceholderText('local'), 'razer')
    await user.click(screen.getByRole('button', { name: /create machine/i }))

    expect(mutateAsync).toHaveBeenCalledTimes(1)
    const body = mutateAsync.mock.calls[0][0]
    expect(body).toEqual({ name: 'razer', provider: 'mock' })
    expect(body).not.toHaveProperty('capacity')
  })
})
