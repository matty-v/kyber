/*
 * Phase → lifecycle-actions-applicable mapping for the agent detail page.
 *
 * Since #128 moved all lifecycle actions into the More dropdown (Restart
 * session is the only header-level primary now), this helper just returns
 * the per-phase applicable subset. The old primaryActionForPhase was
 * removed in that issue — see pwa/src/pages/AgentDetail.tsx for the
 * single-primary-button layout.
 */

import type { AgentPhase } from '../types'

// Actions that operate on the agent's live session and leave the pod alone.
// The "Agent actions" section of the More menu.
export type AgentSessionKind = 'compact-session' | 'restart-session'

// Per-phase session actions. Both need a live runtime to talk to, so both
// are Running-only — on any other phase there is no tmux session to act on
// and the section renders nothing (no empty header).
//
// Order is deliberate and is the order rendered: compact before restart,
// because compacting is the smaller move and the one an operator should
// reach for first. Restart session is the escalation when compaction isn't
// enough.
//
// Runtime capability is NOT decided here — a runtime without compaction
// answers 501 from the API. Hiding the item per-runtime would mean teaching
// the PWA the runtime registry, which the server deliberately owns.
export function sessionItemsInMore(phase: AgentPhase): AgentSessionKind[] {
  return phase === 'Running' ? ['compact-session', 'restart-session'] : []
}

export type AgentLifecycleKind =
  | 'start'
  | 'stop'
  | 'restart'
  // Operator-forced re-auth for a wedged agent (#395): drops it to NeedsAuth
  // (deleting any live pod) so it can be re-authorized from scratch.
  | 'force-needs-auth'
  // Operator "try again with what you have" for a NeedsAuth agent (kyber#26).
  // Distinct kind rather than reusing 'start' because the label, icon and
  // confirm copy are different — but it fires the SAME /start endpoint, which
  // is the one that actually works out of NeedsAuth (see below).
  | 'retry-startup'

// The API sub-action each lifecycle kind fires. Lives here, beside the
// per-phase rules, because "which endpoint" and "which phases" are the same
// decision: an action is only safe to offer on a phase where its endpoint
// actually transitions (the #599 rule). Splitting them across two files is how
// a menu item and its handler drift into a dead button.
//
// 'retry-startup' → 'start' is the whole point of the kyber#26 kind: it reads
// as "Restart pod" to the operator but must fire desiredPhase=Running, the only
// edge out of NeedsAuth. Wiring it to 'restart' would be the silent no-op.
const LIFECYCLE_KINDS: readonly AgentLifecycleKind[] = [
  'start',
  'stop',
  'restart',
  'force-needs-auth',
  'retry-startup',
]

export function isLifecycleKind(kind: string): kind is AgentLifecycleKind {
  return (LIFECYCLE_KINDS as readonly string[]).includes(kind)
}

// The return type is narrowed to the real sub-actions rather than `string` so a
// typo'd dispatch literal at the call site is a tsc error instead of a branch
// that silently never runs — `if (action === 'stpo')` compiles fine against
// `string`. 'retry-startup' is excluded because it is a UI kind, never an
// endpoint: it is precisely the value this function exists to translate away.
export type AgentLifecycleEndpoint = Exclude<AgentLifecycleKind, 'retry-startup'>

export function lifecycleActionEndpoint(kind: AgentLifecycleKind): AgentLifecycleEndpoint {
  switch (kind) {
    case 'retry-startup':
      return 'start'
    case 'start':
    case 'stop':
    case 'restart':
    case 'force-needs-auth':
      return kind
  }
}

// Per-phase lifecycle subsets. Rules:
//   - Don't offer Start on a Running agent; don't offer Stop on a
//     Stopped agent.
//   - Restart (pod roll) is offered where it actually fires — i.e. from
//     Running (EventDesiredRestarting only transitions {Running → Restarting},
//     state_machine.go:213). It is NOT offered from crashed phases, where it
//     matched no transition and silently misled operators (#599).
//   - A crashed agent (Failed/MemoryExhausted) offers the WORKING recovery,
//     'start' → desiredPhase=Running, a valid transition from both
//     ({Failed, EventDesiredRunning} → Starting at state_machine.go:271, and
//     MemoryExhausted is in the EventDesiredRunning allowlist) that recreates
//     the pod — so a crashed agent recovers from the PWA alone, no CLI (#599).
//   - Transient phases (Stopping/Restarting/Creating) show no lifecycle
//     actions; the operator waits for the phase to settle.
//   - force-needs-auth (#395) is offered from every recoverable phase a wedged
//     agent can be stuck in — including Starting (a stuck-Starting agent is a
//     prime re-auth target) and the crashed phases. NeedsAuth itself is excluded
//     (can't re-force what's already there); Deleted has nothing actionable.
//   - NeedsAuth offers 'retry-startup' (kyber#26). It used to be actionless, so
//     an agent parked on a credential that was already good had no control in
//     the PWA at all and needed a cluster-admin `kubectl annotate` to unwedge
//     (the `lando` incident, 2026-08-09). The item is 'retry-startup' and NOT
//     'restart' because #599's rule still binds: EventDesiredRestarting has no
//     transition out of NeedsAuth, so a "Restart pod" wired to /restart would be
//     a dead button. 'retry-startup' fires POST /start → desiredPhase=Running,
//     which the API pairs with clearing status.recoveryInput (routes_agents.go,
//     setAgentDesiredPhase) so the kyber#684 latch opens for exactly one attempt
//     — even when the credential Secret's resourceVersion is unchanged, which is
//     precisely how `lando` was stuck. The controller's AUTOMATIC path is
//     untouched: it still refuses to self-retry on an unchanged credential.
//     One click = one pod create; a still-dead credential lands back here.
//     MemoryExhausted needs nothing equivalent — it already offers 'start',
//     which goes through the same latch-clearing endpoint.
export function lifecycleItemsInMore(phase: AgentPhase): AgentLifecycleKind[] {
  switch (phase) {
    case '':
      return []
    case 'Running':
      return ['stop', 'restart', 'force-needs-auth']
    case 'Stopped':
      return ['start', 'restart', 'force-needs-auth']
    case 'Failed':
    case 'MemoryExhausted':
      return ['start', 'force-needs-auth']
    case 'DiskExhausted':
      // Recovery is automatic after Shell cleanup or a PVC increase. Keep the
      // explicit escape hatches available without offering a redundant Start.
      return ['stop', 'force-needs-auth']
    case 'Starting':
      return ['force-needs-auth']
    case 'NeedsAuth':
      return ['retry-startup']
    case 'Stopping':
    case 'Restarting':
    case 'Creating':
    case 'Draining':
    case 'Deleted':
      return []
    case 'WaitingForMachine':
      return ['stop']
  }
}
