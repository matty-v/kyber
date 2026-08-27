import type { CreateMachineRequest } from '../lib/types'

export type ManagedMachineFormState = {
  name: string
  machineType: string
  diskSizeGb: string // stored as string for <select>, coerced on submit
  zone: string
  spot: boolean
}

export type MockFormState = {
  name: string
}

export function buildManagedMachineRequest(form: ManagedMachineFormState, provider: 'gce' | 'gke' | 'eks' | 'fake'): CreateMachineRequest {
  const request: CreateMachineRequest = {
    name: form.name,
    provider,
    profile: form.machineType,
    interruptible: form.spot,
    location: form.zone,
  }
  if (provider !== 'gke') request.diskSizeGb = parseInt(form.diskSizeGb, 10)
  return request
}

// buildMockRequest produces the minimal body — name + provider only. Since
// kyber#240, the server auto-fills capacity from node.status.allocatable
// (minus the controlPlane.platformReservation carve-out). The operator
// no longer picks CPU/Memory/Disk numbers; the laptop's actual hardware
// dictates what's available.
export function buildMockRequest(form: MockFormState, provider: 'static' | 'mock' = 'mock'): CreateMachineRequest {
  return {
    name: form.name,
    provider,
  }
}

// Validation errors. Return null when valid, otherwise a human-readable string.
export function validateMockForm(form: MockFormState): string | null {
  if (!form.name) return 'Name is required'
  return null
}

export function validateManagedMachineForm(form: ManagedMachineFormState): string | null {
  if (!form.name) return 'Name is required'
  if (!form.machineType) return 'Machine profile is required'
  if (!form.zone) return 'Location is required'
  const disk = parseInt(form.diskSizeGb, 10)
  if (isNaN(disk) || disk < 10) return 'Disk size must be at least 10 GB'
  return null
}
