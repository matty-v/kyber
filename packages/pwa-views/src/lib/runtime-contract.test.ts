import { describe, expect, it } from 'vitest'
import { wizardAuth, wizardApiKey } from './runtime-contract'
import { initialWizardState } from '../components/wizard/types'
import { isAuthValid } from '../components/wizard/validation'

describe('runtime discovery contract', () => {
  it('uses a new harness auth field without borrowing provider behavior', () => {
    const state = { ...initialWizardState([]), runtime: 'fixture', authType: 'api-key' as const, runtimeApiKey: 'fixture-value', runtimes: [{ id: 'fixture', name: 'Fixture', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [{ id: 'api-key' as const, name: 'Fixture key', flow: 'api-key', inputField: 'fixtureKey' }] }] }
    expect(wizardAuth(state)?.inputField).toBe('fixtureKey')
    expect(wizardApiKey(state)).toBe('fixture-value')
    expect(isAuthValid(state).ok).toBe(true)
  })
  it('respects an explicitly empty installation catalog', () => {
    expect(wizardAuth({ ...initialWizardState([]), runtimes: [] })).toBeUndefined()
  })
  it('does not give an unknown harness Claude authentication', () => {
    expect(wizardAuth({ ...initialWizardState([]), runtime: 'unknown' })).toBeUndefined()
  })
})
