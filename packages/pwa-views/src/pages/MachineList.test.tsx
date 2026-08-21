import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { isMachineStandby, MachineAvailableCell, MachineStatusBadge } from './MachineList'
import type { Agent, Machine } from '../lib/types'

function machine(over: Partial<Machine> = {}): Machine {
  return {
    id: 'razer',
    phase: 'Running',
    spec: { provider: 'mock', capacity: { cpu: '14', memory: '28Gi' } },
    status: { phase: 'Running' },
    createdAt: new Date(0).toISOString(),
    ...over,
  }
}

function agent(over: Partial<Agent> = {}): Agent {
  return {
    id: 'alice',
    phase: 'Running',
    machine: 'razer',
    runtime: 'codex',
    model: '',
    scaling: 'warm',
    resources: { cpu: '1', memory: '2Gi', disk: '50Gi' },
    status: { phase: 'Running' },
    ...over,
  }
}

function regionalPendingMachine(): Machine {
  return machine({
    phase: 'Provisioning',
    spec: {
      provider: 'gke',
      managementMode: 'Managed',
      location: 'us-central1',
      capacity: { cpu: '8', memory: '32Gi' },
    },
    status: { phase: 'Provisioning', availability: 'Recovering' },
  })
}

describe('MachineStatusBadge', () => {
  it('shows idle regional managed GKE capacity as Standby', () => {
    const pending = regionalPendingMachine()
    expect(isMachineStandby(pending, [])).toBe(true)

    render(<MachineStatusBadge machine={pending} agents={[]} />)
    expect(screen.getByText('Standby')).toBeInTheDocument()
    expect(screen.getByText(/starts on Agent demand/i)).toBeInTheDocument()
  })

  it('keeps Provisioning when an active Agent is waiting for the machine', () => {
    const pending = regionalPendingMachine()
    expect(isMachineStandby(pending, [agent({ phase: 'WaitingForMachine' })])).toBe(false)

    render(
      <MachineStatusBadge
        machine={pending}
        agents={[agent({ phase: 'WaitingForMachine' })]}
      />,
    )
    expect(screen.getByText('Provisioning')).toBeInTheDocument()
  })

  it('keeps Provisioning until Agent demand is known', () => {
    expect(isMachineStandby(regionalPendingMachine(), undefined)).toBe(false)
  })

  it('does not call a fixed-size zonal GKE machine Standby', () => {
    const pending = regionalPendingMachine()
    pending.spec.location = 'us-central1-a'
    expect(isMachineStandby(pending, [])).toBe(false)
  })

  it('ignores stopped and suspended Agent assignments', () => {
    const pending = regionalPendingMachine()
    expect(isMachineStandby(pending, [agent({ phase: 'Stopped' })])).toBe(true)
    expect(isMachineStandby(pending, [agent({ phase: 'Suspended' })])).toBe(true)
  })
})

describe('MachineAvailableCell', () => {
  it('renders CPU + Memory free/total when assignable + available are populated', () => {
    render(
      <MachineAvailableCell
        machine={machine({
          status: {
            phase: 'Running',
            assignableCapacity: { cpu: '12', memory: '24Gi' },
            availableCapacity: { cpu: '10', memory: '20Gi' },
          },
        })}
      />,
    )
    // CPU row: 10 / 12 free
    expect(screen.getByText(/10\.00 \/ 12\.00 free/i)).toBeInTheDocument()
    // Mem row: 20 / 24 GiB free
    expect(screen.getByText(/20\.0 \/ 24\.0 GiB free/i)).toBeInTheDocument()
    // No disk row when ephemeralStorage isn't on the wire (pre-#129 PR-C).
    expect(screen.queryByText(/Disk/i)).not.toBeInTheDocument()
  })

  it('renders Disk row when ephemeralStorage is populated on assignable + available', () => {
    render(
      <MachineAvailableCell
        machine={machine({
          status: {
            phase: 'Running',
            assignableCapacity: { cpu: '12', memory: '24Gi', ephemeralStorage: '190Gi' },
            availableCapacity: { cpu: '10', memory: '20Gi', ephemeralStorage: '140Gi' },
          },
        })}
      />,
    )
    // Disk row: 140 / 190 GiB free
    expect(screen.getByText(/140\.0 \/ 190\.0 GiB free/i)).toBeInTheDocument()
  })

  it('omits Disk row when ephemeralStorage missing from one side (transitional)', () => {
    render(
      <MachineAvailableCell
        machine={machine({
          status: {
            phase: 'Running',
            assignableCapacity: { cpu: '12', memory: '24Gi', ephemeralStorage: '190Gi' },
            // available didn't surface ephemeralStorage yet — don't half-render.
            availableCapacity: { cpu: '10', memory: '20Gi' },
          },
        })}
      />,
    )
    expect(screen.queryByText(/Disk/i)).not.toBeInTheDocument()
  })

  it('falls back to spec.capacity total when assignable/available are missing', () => {
    render(
      <MachineAvailableCell
        machine={machine({
          spec: { provider: 'mock', capacity: { cpu: '8', memory: '16Gi' } },
          status: { phase: 'Provisioning' }, // no assignable/available yet
        })}
      />,
    )
    expect(screen.getByText(/capacity 8\.00 cpu · 16\.0 Gi/i)).toBeInTheDocument()
  })

  it('renders an em-dash when no capacity data is available at all', () => {
    render(
      <MachineAvailableCell
        machine={machine({
          spec: { provider: 'mock' }, // no capacity at all
          status: { phase: 'Provisioning' },
        })}
      />,
    )
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('uses red band at 90% usage on the matching resource', () => {
    const { container } = render(
      <MachineAvailableCell
        machine={machine({
          status: {
            phase: 'Running',
            assignableCapacity: { cpu: '10', memory: '20Gi' },
            // 1 vCPU free out of 10 = 90% used
            availableCapacity: { cpu: '1', memory: '2Gi' },
          },
        })}
      />,
    )
    // Both bars are red (CPU 90%, Mem 90%).
    const bands = Array.from(
      container.querySelectorAll('[data-band]'),
    ).map((el) => el.getAttribute('data-band'))
    expect(bands).toEqual(['red', 'red'])
  })

  it('uses yellow band at 70% usage', () => {
    const { container } = render(
      <MachineAvailableCell
        machine={machine({
          status: {
            phase: 'Running',
            assignableCapacity: { cpu: '10', memory: '20Gi' },
            // 2.5 vCPU free out of 10 = 75% used → yellow
            // 4 GiB free out of 20 = 80% used → yellow
            availableCapacity: { cpu: '2.5', memory: '4Gi' },
          },
        })}
      />,
    )
    const bands = Array.from(
      container.querySelectorAll('[data-band]'),
    ).map((el) => el.getAttribute('data-band'))
    expect(bands).toEqual(['yellow', 'yellow'])
  })

  it('uses green band when usage is below 70%', () => {
    const { container } = render(
      <MachineAvailableCell
        machine={machine({
          status: {
            phase: 'Running',
            assignableCapacity: { cpu: '10', memory: '20Gi' },
            // 5 vCPU free out of 10 = 50% used → green
            availableCapacity: { cpu: '5', memory: '15Gi' },
          },
        })}
      />,
    )
    const bands = Array.from(
      container.querySelectorAll('[data-band]'),
    ).map((el) => el.getAttribute('data-band'))
    expect(bands).toEqual(['green', 'green'])
  })

  it('renders mixed bands when CPU + Memory are at different fill levels', () => {
    // CPU 50% (green), Memory 95% (red) — proves each bar gets its own
    // band, not just whichever the first one rendered.
    const { container } = render(
      <MachineAvailableCell
        machine={machine({
          status: {
            phase: 'Running',
            assignableCapacity: { cpu: '10', memory: '20Gi' },
            availableCapacity: { cpu: '5', memory: '1Gi' },
          },
        })}
      />,
    )
    const bands = Array.from(
      container.querySelectorAll('[data-band]'),
    ).map((el) => el.getAttribute('data-band'))
    expect(bands).toEqual(['green', 'red'])
  })
})
