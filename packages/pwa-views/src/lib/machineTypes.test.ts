import { describe, it, expect } from 'vitest'
import { availableFromMachine, lookupMachineCapacity, machineCapacity, parseCpu, parseMemoryGi } from './machineTypes'
import type { Agent, Machine } from './types'

describe('parseCpu', () => {
  it('handles millicpu', () => expect(parseCpu('500m')).toBe(0.5))
  it('handles whole cores', () => expect(parseCpu('2')).toBe(2))
  it('handles fractions', () => expect(parseCpu('0.5')).toBe(0.5))
  it('returns 0 for empty', () => expect(parseCpu('')).toBe(0))
  it('returns 0 for garbage', () => expect(parseCpu('abc')).toBe(0))
  it('returns 0 for bare suffix', () => expect(parseCpu('m')).toBe(0))
})

describe('parseMemoryGi', () => {
  it('handles Gi', () => expect(parseMemoryGi('2Gi')).toBe(2))
  it('handles Mi', () => expect(parseMemoryGi('512Mi')).toBeCloseTo(0.5, 3))
  // Node.status.allocatable returns memory as Ki — k8s normalizes to the
  // smallest binary unit. e2-standard-2 has ~7.75Gi allocatable, ~8126592Ki.
  it('handles Ki', () => expect(parseMemoryGi('8126592Ki')).toBeCloseTo(7.75, 1))
})

describe('lookupMachineCapacity', () => {
  // Capacity returned subtracts SystemOverhead (0.25 cpu / 0.25 Gi) so the badge
  // reflects what's actually available for agent pods, not the nominal SKU spec.
  it('returns known type minus system overhead', () =>
    expect(lookupMachineCapacity('e2-standard-2')).toEqual({ cpu: 1.75, memoryGi: 7.75 }))
  it('returns null for unknown', () => expect(lookupMachineCapacity('mystery-9000')).toBeNull())
})

describe('machineCapacity', () => {
  it('returns spec.capacity as-is on mock (no SystemOverhead subtraction)', () => {
    const m: Machine = {
      id: 'local',
      phase: 'Ready',
      spec: {
        provider: 'mock',
        capacity: { cpu: '4', memory: '16Gi' },
      },
      status: { phase: 'Ready' },
      createdAt: '2026-04-17T00:00:00Z',
    }
    expect(machineCapacity(m)).toEqual({ cpu: 4, memoryGi: 16 })
  })

  it('falls back to machineType lookup when capacity is absent', () => {
    const m: Machine = {
      id: 'w',
      phase: 'Ready',
      spec: {
        provider: 'gce',
        machineType: 'e2-standard-2',
        diskSizeGb: 50,
        spot: false,
        zone: 'us-central1-a',
      },
      status: { phase: 'Ready' },
      createdAt: '2026-04-17T00:00:00Z',
    }
    // lookupMachineCapacity subtracts SystemOverhead (0.25 / 0.25Gi)
    expect(machineCapacity(m)).toEqual({ cpu: 1.75, memoryGi: 7.75 })
  })

  it('prefers spec.capacity over machineType when both are present (gce post-Phase-B)', () => {
    const m: Machine = {
      id: 'w',
      phase: 'Ready',
      spec: {
        provider: 'gce',
        capacity: { cpu: '4', memory: '16Gi' },
        machineType: 'n2-standard-4',
        diskSizeGb: 50,
        spot: false,
        zone: 'us-central1-a',
      },
      status: { phase: 'Ready' },
      createdAt: '2026-04-17T00:00:00Z',
    }
    // spec.capacity wins — no overhead subtraction.
    expect(machineCapacity(m)).toEqual({ cpu: 4, memoryGi: 16 })
  })

  it('returns null when neither capacity nor machineType is present', () => {
    const m: Machine = {
      id: 'mystery',
      phase: 'Provisioning',
      spec: { provider: 'gce' }, // pre-Phase-B legacy, not yet reconciled
      status: { phase: 'Provisioning' },
      createdAt: '2026-04-17T00:00:00Z',
    }
    expect(machineCapacity(m)).toBeNull()
  })
})

describe('availableFromMachine', () => {
  // Three-tier preference: status.availableCapacity → status.availableCPU/Memory
  // → spec.capacity minus agents. Disk follows the same tier order but degrades
  // to 0 (not null) when ephemeralStorage isn't on the wire (#129 PR-C).
  const baseMachine: Machine = {
    id: 'razer',
    phase: 'Running',
    spec: { provider: 'mock', capacity: { cpu: '16', memory: '32Gi' } },
    status: { phase: 'Running' },
    createdAt: '2026-04-17T00:00:00Z',
  }

  it('reads diskGi from status.availableCapacity.ephemeralStorage (tier 1)', () => {
    const m: Machine = {
      ...baseMachine,
      status: {
        phase: 'Running',
        availableCapacity: { cpu: '10', memory: '20Gi', ephemeralStorage: '140Gi' },
      },
    }
    expect(availableFromMachine(m, [])).toEqual({ cpu: 10, memoryGi: 20, diskGi: 140 })
  })

  it('returns diskGi=0 when tier-1 availableCapacity lacks ephemeralStorage', () => {
    // Pre-#129 PR-C cluster: availableCapacity present but no ephemeralStorage.
    const m: Machine = {
      ...baseMachine,
      status: {
        phase: 'Running',
        availableCapacity: { cpu: '10', memory: '20Gi' },
      },
    }
    expect(availableFromMachine(m, [])).toEqual({ cpu: 10, memoryGi: 20, diskGi: 0 })
  })

  it('returns diskGi=0 from tier-2 legacy availableCPU/availableMemory (no disk on legacy)', () => {
    const m: Machine = {
      ...baseMachine,
      status: {
        phase: 'Running',
        availableCPU: '10',
        availableMemory: '20Gi',
      },
    }
    expect(availableFromMachine(m, [])).toEqual({ cpu: 10, memoryGi: 20, diskGi: 0 })
  })

  it('falls back to spec - agents (tier 3); disk degrades to 0 since spec has no disk', () => {
    const agents: Agent[] = [
      {
        machine: 'razer',
        resources: { cpu: '4', memory: '8Gi', disk: '50Gi' },
      } as Agent,
    ]
    const m: Machine = {
      ...baseMachine,
      // No availableCapacity, no legacy fields — pure spec - agents.
      status: { phase: 'Running' },
    }
    // CPU: 16 - 4 = 12; Mem: 32 - 8 = 24; Disk: 0 - 50 → clamped to 0.
    expect(availableFromMachine(m, agents)).toEqual({ cpu: 12, memoryGi: 24, diskGi: 0 })
  })

  it('returns null when no tier produces a number (no spec.capacity, no machineType)', () => {
    const m: Machine = {
      id: 'mystery',
      phase: 'Provisioning',
      spec: { provider: 'gce' },
      status: { phase: 'Provisioning' },
      createdAt: '2026-04-17T00:00:00Z',
    }
    expect(availableFromMachine(m, [])).toBeNull()
  })
})
