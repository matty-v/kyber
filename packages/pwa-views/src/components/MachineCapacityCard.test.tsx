import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MachineCapacityCard } from './MachineCapacityCard'
import type { Machine } from '../lib/types'

const fullyPopulated: Machine = {
  id: 'razer',
  status: {
    observedCapacity:   { cpu: '4', memory: '8Gi', ephemeralStorage: '200Gi' },
    assignableCapacity: { cpu: '3', memory: '7Gi', ephemeralStorage: '180Gi' },
    availableCapacity:  { cpu: '1500m', memory: '4Gi', ephemeralStorage: '120Gi' },
  } as Machine['status'],
} as unknown as Machine

const noDisk: Machine = {
  id: 'old-controller',
  status: {
    assignableCapacity: { cpu: '4', memory: '8Gi' },
    availableCapacity:  { cpu: '2', memory: '4Gi' },
  } as Machine['status'],
} as unknown as Machine

describe('MachineCapacityCard', () => {
  it('renders three resource rows (CPU, Memory, Disk) when fully populated', () => {
    render(<MachineCapacityCard machine={fullyPopulated} />)
    expect(screen.getByText('CPU')).toBeInTheDocument()
    expect(screen.getByText('Memory')).toBeInTheDocument()
    expect(screen.getByText('Disk')).toBeInTheDocument()
  })

  it('shows free/total numbers per resource', () => {
    render(<MachineCapacityCard machine={fullyPopulated} />)
    // CPU: 1500m free out of 3 total -> 1.50 / 3.00 free
    expect(screen.getByLabelText(/CPU: 1\.50 free of 3\.00/i)).toBeInTheDocument()
    // Memory: 4 GiB free of 7 GiB
    expect(screen.getByLabelText(/Memory: 4\.0 GiB free of 7\.0 GiB/i)).toBeInTheDocument()
    // Disk: 120 GiB free of 180 GiB
    expect(screen.getByLabelText(/Disk: 120\.0 GiB free of 180\.0 GiB/i)).toBeInTheDocument()
  })

  it('omits the Disk row when ephemeralStorage is missing on either side', () => {
    render(<MachineCapacityCard machine={noDisk} />)
    expect(screen.getByText('CPU')).toBeInTheDocument()
    expect(screen.getByText('Memory')).toBeInTheDocument()
    expect(screen.queryByText('Disk')).not.toBeInTheDocument()
  })

  it('shows a friendly placeholder when capacity has not been reported yet', () => {
    const noCapacityMachine: Machine = {
      id: 'unknown',
      status: {} as Machine['status'],
    } as unknown as Machine
    render(<MachineCapacityCard machine={noCapacityMachine} />)
    expect(screen.getByText(/not yet reported/i)).toBeInTheDocument()
    // No bar rows when capacity is missing.
    expect(screen.queryByText('CPU')).not.toBeInTheDocument()
    expect(screen.queryByText('Memory')).not.toBeInTheDocument()
    expect(screen.queryByText('Disk')).not.toBeInTheDocument()
  })

  it('does not surface internal data-model jargon', () => {
    render(<MachineCapacityCard machine={fullyPopulated} />)
    expect(screen.queryByText('Observed')).not.toBeInTheDocument()
    expect(screen.queryByText('Reservation')).not.toBeInTheDocument()
    expect(screen.queryByText('Assignable')).not.toBeInTheDocument()
    expect(screen.queryByText('Assigned')).not.toBeInTheDocument()
  })
})
