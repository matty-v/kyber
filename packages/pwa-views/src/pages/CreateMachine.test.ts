import { describe, it, expect } from 'vitest'
import {
  buildGceRequest,
  buildMockRequest,
  validateGceForm,
  validateMockForm,
} from './CreateMachine.helpers'

describe('buildGceRequest', () => {
  it('coerces diskSizeGb to number', () => {
    const got = buildGceRequest({
      name: 'worker-1',
      machineType: 'n2-standard-4',
      diskSizeGb: '50',
      zone: 'us-central1-a',
      spot: false,
    })
    expect(got).toEqual({
      name: 'worker-1',
      provider: 'gce',
      machineType: 'n2-standard-4',
      diskSizeGb: 50,
      zone: 'us-central1-a',
      spot: false,
    })
  })

  it('preserves spot flag', () => {
    const got = buildGceRequest({
      name: 'w', machineType: 'e2-small', diskSizeGb: '20',
      zone: 'us-west1-a', spot: true,
    })
    expect(got.provider).toBe('gce')
    if (got.provider === 'gce') expect(got.spot).toBe(true)
  })
})

describe('buildMockRequest', () => {
  it('produces {name, provider:"mock"} with no capacity', () => {
    const got = buildMockRequest({ name: 'local' })
    expect(got).toEqual({ name: 'local', provider: 'mock' })
    // Belt-and-suspenders: assert capacity is genuinely absent so the
    // server's auto-fill from node.allocatable kicks in (kyber#240).
    expect(got).not.toHaveProperty('capacity')
  })

  it('passes the name through verbatim', () => {
    const got = buildMockRequest({ name: 'razer' })
    expect(got.name).toBe('razer')
    expect(got.provider).toBe('mock')
  })
})

describe('validateMockForm', () => {
  it('passes when name is present', () => {
    expect(validateMockForm({ name: 'local' })).toBeNull()
  })
  it('rejects empty name', () => {
    expect(validateMockForm({ name: '' })).toMatch(/name/i)
  })
})

describe('validateGceForm', () => {
  const ok = { name: 'w', machineType: 'e2-small', diskSizeGb: '50', zone: 'us-central1-a', spot: false }
  it('passes complete form', () => {
    expect(validateGceForm(ok)).toBeNull()
  })
  it('rejects disk < 10GB', () => {
    expect(validateGceForm({ ...ok, diskSizeGb: '5' })).toMatch(/disk/i)
  })
  it('rejects empty name', () => {
    expect(validateGceForm({ ...ok, name: '' })).toMatch(/name/i)
  })
})
