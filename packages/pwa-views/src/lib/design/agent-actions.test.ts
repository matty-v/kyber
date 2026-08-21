import { describe, it, expect } from 'vitest'
import {
  isLifecycleKind,
  lifecycleActionEndpoint,
  lifecycleItemsInMore,
  sessionItemsInMore,
} from './agent-actions'

// Per-phase lifecycle menu contents (#128 — post-restructure: Restart
// session is the lone header primary, everything else lives in More).
// #395 adds the operator-forced re-auth action ('force-needs-auth') to the
// recoverable phases and models MemoryExhausted in the PWA AgentPhase union.
// #599: a crashed agent (Failed/MemoryExhausted) now offers the WORKING
// recovery — 'start' (desiredPhase=Running) — and no longer the no-op
// 'restart' (Restarting only transitions from Running; state_machine.go:213).
// kyber#26 applies the same correction to NeedsAuth, which was actionless: it
// gains 'retry-startup', which fires /start (the edge that exists) rather than
// /restart (the edge that does not).

describe('lifecycleItemsInMore', () => {
  it('has no actions before the controller reports the initial phase', () => {
    expect(lifecycleItemsInMore('')).toEqual([])
  })

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

  it('NeedsAuth: retry-startup — the wedged-agent control that used to require kubectl (kyber#26)', () => {
    expect(lifecycleItemsInMore('NeedsAuth')).toEqual(['retry-startup'])
  })

  it('NeedsAuth does NOT offer restart — EventDesiredRestarting has no edge out of NeedsAuth (#599 rule)', () => {
    // The whole point of the separate kind. 'restart' POSTs
    // desiredPhase=Restarting, which matches no transition row from NeedsAuth
    // (state_machine.go — the only Restarting row is {Running → Restarting}),
    // so offering it here would ship the dead button #599 removed elsewhere.
    expect(lifecycleItemsInMore('NeedsAuth')).not.toContain('restart')
  })

  it('retry-startup is offered ONLY from NeedsAuth — every other phase reaches /start by its own action', () => {
    const others = [
      'Running',
      'Stopped',
      'Suspended',
      'Failed',
      'MemoryExhausted',
      'Starting',
      'Stopping',
      'Restarting',
      'Creating',
      'Deleted',
    ] as const
    for (const phase of others) {
      expect(lifecycleItemsInMore(phase), `${phase} must not offer retry-startup`).not.toContain(
        'retry-startup',
      )
    }
  })

  it.each(['Stopping', 'Restarting', 'Creating'] as const)(
    'no actions during transient phase %s',
    (phase) => {
      expect(lifecycleItemsInMore(phase)).toEqual([])
    },
  )

  it('no lifecycle actions for Deleted', () => {
    expect(lifecycleItemsInMore('Deleted')).toEqual([])
  })
})

// The kind→endpoint mapping. Chewie's review of kyber#69 caught that nothing
// pinned this: the menu item fired 'retry-startup' and a test proved it, but
// the handler's choice of mutation was untested, so swapping it to the restart
// mutation left all 661 tests green — reintroducing exactly the dead button
// this issue exists to prevent.
describe('lifecycleActionEndpoint', () => {
  it('retry-startup fires /start — the edge that exists out of NeedsAuth (kyber#26)', () => {
    expect(lifecycleActionEndpoint('retry-startup')).toBe('start')
  })

  it('retry-startup must NOT fire /restart — that is the silent no-op (#599)', () => {
    // desiredPhase=Restarting matches no transition row from NeedsAuth, so this
    // assertion is the guard against a one-word edit turning the button dead.
    expect(lifecycleActionEndpoint('retry-startup')).not.toBe('restart')
  })

  it.each(['start', 'stop', 'restart', 'suspend', 'force-needs-auth'] as const)(
    '%s fires its own like-named endpoint',
    (kind) => {
      expect(lifecycleActionEndpoint(kind)).toBe(kind)
    },
  )

  it('every kind offered on any phase resolves to an endpoint', () => {
    const phases = [
      'Running',
      'Stopped',
      'Suspended',
      'Failed',
      'MemoryExhausted',
      'Starting',
      'NeedsAuth',
      'Stopping',
      'Restarting',
      'Creating',
      'Deleted',
    ] as const
    for (const phase of phases) {
      for (const kind of lifecycleItemsInMore(phase)) {
        expect(lifecycleActionEndpoint(kind), `${phase} → ${kind}`).toBeTruthy()
      }
    }
  })
})

describe('isLifecycleKind', () => {
  it('accepts every lifecycle kind and rejects the session/setter kinds', () => {
    for (const kind of ['start', 'stop', 'restart', 'suspend', 'force-needs-auth', 'retry-startup']) {
      expect(isLifecycleKind(kind), kind).toBe(true)
    }
    // These must fall through the guard untouched — routing them through
    // lifecycleActionEndpoint would be a category error.
    for (const kind of ['restart-session', 'compact-session', 'delete', 'set-model']) {
      expect(isLifecycleKind(kind), kind).toBe(false)
    }
  })
})

// Per-phase "Agent actions" section — the session-scoped half of the More
// menu (compact / restart session), split out from the pod-scoped half when
// the menu grew sections. Both need a live runtime to paste into, so both
// are Running-only; every other phase returns [] and the section's header
// is suppressed rather than rendering an empty group.
describe('sessionItemsInMore', () => {
  it('has no actions before the controller reports the initial phase', () => {
    expect(sessionItemsInMore('')).toEqual([])
  })

  it('Running: compact before restart — the smaller move comes first', () => {
    expect(sessionItemsInMore('Running')).toEqual(['compact-session', 'restart-session'])
  })

  it.each([
    'Stopped',
    'Suspended',
    'Failed',
    'MemoryExhausted',
    'Starting',
    'Stopping',
    'Restarting',
    'Creating',
    'NeedsAuth',
    'Deleted',
  ] as const)('no session actions for %s — there is no live session to act on', (phase) => {
    expect(sessionItemsInMore(phase)).toEqual([])
  })

  // The section header is driven off this array's length, so a non-empty
  // result on a phase with no runtime would render actions that can only
  // 409. Stated as its own assertion because it is the failure the
  // conditional header exists to prevent.
  it('is empty on exactly the phases where the API would answer 409', () => {
    const running = sessionItemsInMore('Running')
    expect(running.length).toBeGreaterThan(0)
    expect(sessionItemsInMore('MemoryExhausted')).toHaveLength(0)
  })
})
