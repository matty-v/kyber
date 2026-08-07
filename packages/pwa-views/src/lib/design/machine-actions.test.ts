import { describe, it, expect } from 'vitest'
import { lifecycleItemsInMore } from './machine-actions'
import type { MachinePhase } from '../types'

describe('lifecycleItemsInMore', () => {
  it('Ready/Running offers restart-agents + Stop + Reboot + Delete (no Start)', () => {
    for (const phase of ['Ready', 'Running'] as MachinePhase[]) {
      const more = lifecycleItemsInMore(phase)
      expect(more, `${phase}`).toEqual(['restart-agents', 'stop', 'reboot', 'delete'])
      expect(more).not.toContain('start')
    }
  })

  it('Stopped offers Start + Delete (no Stop, no Reboot)', () => {
    expect(lifecycleItemsInMore('Stopped')).toEqual(['start', 'delete'])
  })

  it('Failed offers Start + Reboot + Delete (Reboot is the recovery hammer)', () => {
    expect(lifecycleItemsInMore('Failed')).toEqual(['start', 'reboot', 'delete'])
  })

  it('always offers Delete except in the Deleted tombstone phase', () => {
    const phases: MachinePhase[] = [
      'Ready',
      'Running',
      'Stopped',
      'Failed',
      'Provisioning',
      'Stopping',
      'Preempted',
      'Replacing',
    ]
    for (const phase of phases) {
      expect(lifecycleItemsInMore(phase), `${phase} should offer Delete`).toContain(
        'delete',
      )
    }
    expect(lifecycleItemsInMore('Deleted')).not.toContain('delete')
  })

  it('transient phases offer only Delete (escape hatch, no actions that race the state machine)', () => {
    const transient: MachinePhase[] = [
      'Provisioning',
      'Stopping',
      'Preempted',
      'Replacing',
    ]
    for (const phase of transient) {
      expect(lifecycleItemsInMore(phase)).toEqual(['delete'])
    }
  })

  it('Deleted phase offers nothing — tombstone', () => {
    expect(lifecycleItemsInMore('Deleted')).toEqual([])
  })
})
