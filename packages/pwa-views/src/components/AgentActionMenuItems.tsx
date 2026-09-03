/*
 * The agent action menu items, shared by the agent LIST and the agent DETAIL
 * page so both surfaces offer the same actions for the same phase.
 *
 * They did not, and that is why this file exists. The detail page rendered
 * from lifecycleItemsInMore(phase); the list hardcoded a static
 * Start/Stop/Restart/Delete menu that never looked at the agent's phase. A
 * Running agent was therefore offered "Start" from the list, and a crashed one
 * was offered "Restart" — the exact dead button kyber#599 removed from the
 * detail page, still live one screen over. The recovery actions a wedged agent
 * needs (Require re-auth, Repair runtime, Restart pod out of NeedsAuth) were
 * missing from the list entirely.
 *
 * agent-actions.ts already warns about this shape: "Splitting them across two
 * files is how a menu item and its handler drift into a dead button." Two
 * copies of the menu is the same failure one level up, so the menu now has one
 * definition and the per-phase rules keep their single owner.
 *
 * Configuration actions (Set Model / Set Harness Version / Set Resources) are
 * deliberately NOT here. They open dialogs with their own form state and
 * belong to the detail page; a row menu in a list is the wrong place to edit
 * an agent's definition.
 */

import { Play, Square, RotateCcw, KeyRound, Wrench, Minimize2 } from 'lucide-react'
import { DropdownMenuItem } from '@/components/ui/dropdown-menu'
import { lifecycleItemsInMore, sessionItemsInMore } from '../lib/design/agent-actions'
import type { AgentLifecycleKind, AgentSessionKind } from '../lib/design/agent-actions'
import type { AgentPhase } from '../lib/types'

/**
 * SessionMenuItems renders the actions that operate on the agent's live
 * session and leave the pod alone. Running-only — sessionItemsInMore returns
 * nothing on any other phase, so this renders nothing rather than an empty
 * section.
 */
export function SessionMenuItems({
  phase,
  onSelect,
}: {
  phase: AgentPhase
  onSelect: (kind: AgentSessionKind) => void
}) {
  const items = sessionItemsInMore(phase)
  return (
    <>
      {items.includes('compact-session') && (
        <DropdownMenuItem
          onSelect={() => onSelect('compact-session')}
          aria-label="Compact session"
        >
          <Minimize2 className="h-3.5 w-3.5" />
          Compact session
        </DropdownMenuItem>
      )}
      {items.includes('restart-session') && (
        <DropdownMenuItem
          onSelect={() => onSelect('restart-session')}
          aria-label="Restart session"
        >
          <RotateCcw className="h-3.5 w-3.5" />
          Restart session
        </DropdownMenuItem>
      )}
    </>
  )
}

/**
 * LifecycleMenuItems renders the per-phase pod actions. The applicable set is
 * owned by lifecycleItemsInMore; this only maps each kind to its labelled
 * item, so an action can never be offered on a phase whose endpoint would not
 * transition (the kyber#599 rule).
 */
export function LifecycleMenuItems({
  phase,
  onSelect,
}: {
  phase: AgentPhase
  onSelect: (kind: AgentLifecycleKind) => void
}) {
  const items = lifecycleItemsInMore(phase)
  return (
    <>
      {items.includes('start') && (
        <DropdownMenuItem onSelect={() => onSelect('start')}>
          <Play className="h-3.5 w-3.5" />
          Start
        </DropdownMenuItem>
      )}
      {items.includes('stop') && (
        <DropdownMenuItem onSelect={() => onSelect('stop')}>
          <Square className="h-3.5 w-3.5" />
          Stop
        </DropdownMenuItem>
      )}
      {items.includes('restart') && (
        <DropdownMenuItem onSelect={() => onSelect('restart')}>
          <RotateCcw className="h-3.5 w-3.5" />
          Restart pod
        </DropdownMenuItem>
      )}
      {items.includes('force-needs-auth') && (
        <DropdownMenuItem onSelect={() => onSelect('force-needs-auth')}>
          <KeyRound className="h-3.5 w-3.5" />
          Require re-auth
        </DropdownMenuItem>
      )}
      {items.includes('repair-runtime') && (
        <DropdownMenuItem onSelect={() => onSelect('repair-runtime')}>
          <Wrench className="h-3.5 w-3.5" />
          Repair runtime
        </DropdownMenuItem>
      )}
      {/* kyber#26: the only lifecycle control a NeedsAuth agent gets. Labelled
          for what the operator sees happen (the pod is rebuilt) rather than for
          the endpoint it calls (/start) — see agent-actions.ts for why it is
          not the 'restart' kind. */}
      {items.includes('retry-startup') && (
        <DropdownMenuItem onSelect={() => onSelect('retry-startup')}>
          <RotateCcw className="h-3.5 w-3.5" />
          Restart pod
        </DropdownMenuItem>
      )}
    </>
  )
}
