import type { CreateMachineRequest } from '../lib/types'

export type GceFormState = {
  name: string
  machineType: string
  diskSizeGb: string // stored as string for <select>, coerced on submit
  zone: string
  spot: boolean
}

export type MockFormState = {
  name: string
}

export function buildGceRequest(form: GceFormState): CreateMachineRequest {
  return {
    name: form.name,
    provider: 'gce',
    machineType: form.machineType,
    diskSizeGb: parseInt(form.diskSizeGb, 10),
    spot: form.spot,
    zone: form.zone,
  }
}

// buildMockRequest produces the minimal body — name + provider only. Since
// kyber#240, the server auto-fills capacity from node.status.allocatable
// (minus the controlPlane.platformReservation carve-out). The operator
// no longer picks CPU/Memory/Disk numbers; the laptop's actual hardware
// dictates what's available.
export function buildMockRequest(form: MockFormState): CreateMachineRequest {
  return {
    name: form.name,
    provider: 'mock',
  }
}

// Validation errors. Return null when valid, otherwise a human-readable string.
export function validateMockForm(form: MockFormState): string | null {
  if (!form.name) return 'Name is required'
  return null
}

export function validateGceForm(form: GceFormState): string | null {
  if (!form.name) return 'Name is required'
  const disk = parseInt(form.diskSizeGb, 10)
  if (isNaN(disk) || disk < 10) return 'Disk size must be at least 10 GB'
  return null
}
