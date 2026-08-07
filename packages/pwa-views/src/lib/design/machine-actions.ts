/*
 * Phase → lifecycle-actions-applicable mapping for the machine detail page.
 *
 * Since #292 collapsed the MachineDetail header to a single row by moving
 * everything into the More dropdown (mirroring AgentDetail's #262 fix),
 * this helper just returns the per-phase applicable subset. The old
 * primaryActionForPhase was removed in #292 — see
 * pwa/src/pages/MachineDetail.tsx for the no-primary-button layout.
 */

import type { MachinePhase } from '../types'

export type MachineLifecycleKind =
  | 'start'
  | 'stop'
  | 'reboot'
  | 'restart-agents'
  | 'delete'

// Per-phase lifecycle subsets. Rules:
//   - Restart-agents is the daily driver on Ready/Running (kicks every agent
//     pod on this node). The dropdown item handles its own per-eligible-agent
//     disabled state — see MachineDetail.tsx.
//   - Don't offer Start on an already-Running machine; don't offer Stop on a
//     Stopped one.
//   - Reboot is the recovery hammer — present on Ready/Running and Failed.
//   - Delete is the trailing destructive action; available in any phase
//     where the operator might want to abandon a stuck machine.
//   - Provisioning / Stopping / Preempted / Replacing show only Delete so
//     operators have an escape hatch but no actions that race the in-flight
//     state machine.
//   - Deleted: tombstone — nothing actionable.
export function lifecycleItemsInMore(phase: MachinePhase): MachineLifecycleKind[] {
  switch (phase) {
    case 'Ready':
    case 'Running':
      return ['restart-agents', 'stop', 'reboot', 'delete']
    case 'Stopped':
      return ['start', 'delete']
    case 'Failed':
      return ['start', 'reboot', 'delete']
    case 'Provisioning':
    case 'Stopping':
    case 'Preempted':
    case 'Replacing':
      return ['delete']
    case 'Deleted':
      return []
  }
}
