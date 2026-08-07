import { describe, it, expect } from 'vitest'
import { lifecycleItemsInMore } from './agent-actions'

// Per-phase lifecycle menu contents (#128 — post-restructure: Restart
// session is the lone header primary, everything else lives in More).
// #395 adds the operator-forced re-auth action ('force-needs-auth') to the
// recoverable phases and models MemoryExhausted in the PWA AgentPhase union.
// #599: a crashed agent (Failed/MemoryExhausted) now offers the WORKING
// recovery — 'start' (desiredPhase=Running) — and no longer the no-op
// 'restart' (Restarting only transitions from Running; state_machine.go:213).

describe('lifecycleItemsInMore', () => {
  it('Running: Stop + Restart pod + Suspend + Require re-auth (no Start)', () => {
    expect(lifecycleItemsInMore('Running')).toEqual([
      'stop',
      'restart',
      'suspend',
      'force-needs-auth',
    ])
  })

  it('Stopped: Start + Restart pod + Require re-auth (no Stop/Suspend)', () => {
    expect(lifecycleItemsInMore('Stopped')).toEqual(['start', 'restart', 'force-needs-auth'])
  })

  it('Suspended: Start (resume) + Restart pod + Require re-auth', () => {
    expect(lifecycleItemsInMore('Suspended')).toEqual(['start', 'restart', 'force-needs-auth'])
  })

  it('Failed: Start (recover) + Require re-auth — offers the working recovery, not the no-op restart (#599)', () => {
    expect(lifecycleItemsInMore('Failed')).toEqual(['start', 'force-needs-auth'])
    // The no-op restart must be gone: Restarting only transitions from Running,
    // so a Restart from a crashed phase matched nothing and silently misled.
    expect(lifecycleItemsInMore('Failed')).not.toContain('restart')
  })

  it('MemoryExhausted: Start (recover) + Require re-auth — the Boba Fett OOM recovery, no CLI (#599)', () => {
    expect(lifecycleItemsInMore('MemoryExhausted')).toEqual(['start', 'force-needs-auth'])
    expect(lifecycleItemsInMore('MemoryExhausted')).not.toContain('restart')
  })

  it('Starting: Require re-auth only — a wedged Starting agent is a re-auth target (#395)', () => {
    expect(lifecycleItemsInMore('Starting')).toEqual(['force-needs-auth'])
  })

  it.each(['Stopping', 'Restarting', 'Creating'] as const)(
    'no actions during transient phase %s',
    (phase) => {
      expect(lifecycleItemsInMore(phase)).toEqual([])
    },
  )

  it.each(['NeedsAuth', 'Deleted'] as const)('no lifecycle actions for %s', (phase) => {
    expect(lifecycleItemsInMore(phase)).toEqual([])
  })
})
