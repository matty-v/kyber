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

export type AgentLifecycleKind =
  | 'start'
  | 'stop'
  | 'restart'
  | 'suspend'
  // Operator-forced re-auth for a wedged agent (#395): drops it to NeedsAuth
  // (deleting any live pod) so it can be re-authorized from scratch.
  | 'force-needs-auth'

// Per-phase lifecycle subsets. Rules:
//   - Don't offer Start on a Running agent; don't offer Stop/Suspend on a
//     Stopped or Suspended agent.
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
export function lifecycleItemsInMore(phase: AgentPhase): AgentLifecycleKind[] {
  switch (phase) {
    case 'Running':
      return ['stop', 'restart', 'suspend', 'force-needs-auth']
    case 'Stopped':
    case 'Suspended':
      return ['start', 'restart', 'force-needs-auth']
    case 'Failed':
    case 'MemoryExhausted':
      return ['start', 'force-needs-auth']
    case 'Starting':
      return ['force-needs-auth']
    case 'Stopping':
    case 'Restarting':
    case 'Creating':
    case 'NeedsAuth':
    case 'Deleted':
      return []
  }
}
