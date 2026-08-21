import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { Machine } from '../lib/types'
import { MachineRecoveryBanner } from './MachineRecoveryBanner'

function machine(overrides: Partial<Machine> = {}): Machine {
  return {
    id: 'coders',
    phase: 'Replacing',
    spec: { provider: 'gke', availabilityClass: 'costOptimized', location: 'zone-a' },
    status: {
      phase: 'Replacing',
      availability: 'Recovering',
      providerRef: 'opaque-ref',
      message: 'provider detail',
    },
    createdAt: '2026-08-21T00:00:00Z',
    ...overrides,
  }
}

describe('MachineRecoveryBanner', () => {
  it('explains automatic replacement and collapses diagnostics', () => {
    render(<MachineRecoveryBanner machine={machine()} />)
    expect(screen.getByText('Machine capacity is recovering')).toBeInTheDocument()
    expect(screen.getByText(/resume its agents automatically/)).toBeInTheDocument()
    expect(screen.getByText('Technical details')).toBeInTheDocument()
    expect(screen.getByText(/providerRef: opaque-ref/)).toBeInTheDocument()
  })

  it('renders nothing for available capacity', () => {
    const { container } = render(<MachineRecoveryBanner machine={machine({
      phase: 'Ready',
      status: { phase: 'Ready', availability: 'Available' },
    })} />)
    expect(container.firstChild).toBeNull()
  })
})
