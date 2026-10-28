import { describe, expect, it } from 'vitest'
import { summarizeCredentialUsage, usageCount, validOperatorCredentialUsage } from './operator-usage'

import { usageCredential, usageView } from '../test/operator-usage-fixture'

describe('operator activity metadata', () => {
  it('preserves integer precision and pre-adoption identity continuity', () => {
    const data = usageView()
    data.items.push({ ...data.items[0], authority: 'configuration', authenticated: '4', denied: '0', generation: '1', lastAcceptedAt: '2026-10-24T12:01:00Z', actions: { 'agents:read': '4' } })
    expect(validOperatorCredentialUsage(data)).toBe(true)
    expect(summarizeCredentialUsage(data, usageCredential)?.authenticated).toBe(9007199254740997n)
    expect(summarizeCredentialUsage(data, usageCredential)?.lastAcceptedAt).toBe('2026-10-24T12:04:00Z')
    expect(summarizeCredentialUsage(data, { ...usageCredential, tenantId: 'another-tenant' })).toBeUndefined()
    expect(summarizeCredentialUsage(data, { ...usageCredential, principalId: 'another-principal' })).toBeUndefined()
  })
  it('rejects malformed counters, duplicated rows and missing observations instead of inventing zeroes', () => {
    expect(usageCount(undefined)).toBeUndefined()
    expect(usageCount('01')).toBeUndefined()
    expect(usageCount('1.5')).toBeUndefined()
    expect(usageCount('9007199254740993')).toBe(9007199254740993n)
    const data = usageView(); data.items[0].authenticated = '1'
    expect(validOperatorCredentialUsage(data)).toBe(false)
    const duplicate = usageView(); duplicate.items.push({ ...duplicate.items[0] })
    expect(validOperatorCredentialUsage(duplicate)).toBe(false)
    const noTime = usageView(); delete noTime.items[0].lastAcceptedAt
    expect(validOperatorCredentialUsage(noTime)).toBe(false)
  })
})
