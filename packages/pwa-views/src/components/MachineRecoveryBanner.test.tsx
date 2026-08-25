import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
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

  it('describes initial on-demand capacity without claiming reclamation', () => {
    render(<MachineRecoveryBanner machine={machine({
      phase: 'Provisioning',
      status: { phase: 'Provisioning', availability: 'Recovering' },
    })} />)
    expect(screen.getByText('Machine capacity is starting')).toBeInTheDocument()
    expect(screen.getByText(/requested provider capacity/)).toBeInTheDocument()
    expect(screen.queryByText(/provider reclaimed/)).not.toBeInTheDocument()
  })

  it('renders nothing for available capacity', () => {
    const { container } = render(<MachineRecoveryBanner machine={machine({
      phase: 'Ready',
      status: { phase: 'Ready', availability: 'Available' },
    })} />)
    expect(container.firstChild).toBeNull()
  })

  it('makes reliable-rate fallback explicit and offers a provider-neutral retry', () => {
    const onRetry = vi.fn()
    render(<MachineRecoveryBanner
      machine={machine({
        phase: 'Running',
        spec: { provider: 'fake', availabilityClass: 'costOptimized', location: 'zone-a' },
        status: {
          phase: 'Running',
          availability: 'Available',
          effectiveAvailabilityClass: 'reliable',
          fallbackReason: 'CostOptimizedUnavailable',
          fallbackSince: '2026-08-25T00:00:00Z',
        },
      })}
      reliableFallbackMode="Automatic"
      onRetryCostOptimized={onRetry}
    />)
    expect(screen.getByText('Running on reliable fallback capacity')).toBeInTheDocument()
    expect(screen.getByText(/reliable-rate capacity/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Retry cost-optimized capacity' }))
    expect(onRetry).toHaveBeenCalledOnce()
  })

  it('explains retry rollback and hides duplicate retry action while in progress', () => {
    render(<MachineRecoveryBanner
      machine={machine({
        phase: 'Replacing',
        spec: {
          provider: 'fake',
          availabilityClass: 'costOptimized',
          location: 'zone-a',
          costOptimizedRetryRequest: 'req-2',
        },
        status: {
          phase: 'Replacing',
          availability: 'Recovering',
          effectiveAvailabilityClass: 'reliable',
          costOptimizedRetryObserved: 'req-1',
        },
      })}
      reliableFallbackMode="Automatic"
      onRetryCostOptimized={() => undefined}
    />)
    expect(screen.getByText('Retrying cost-optimized capacity')).toBeInTheDocument()
    expect(screen.getByText(/return to reliable capacity/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Retry cost-optimized capacity' })).not.toBeInTheDocument()
  })
})
