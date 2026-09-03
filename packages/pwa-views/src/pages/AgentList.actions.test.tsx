import { describe, it, expect, vi } from 'vitest'
import { render, screen, cleanup } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { Agent, AgentPhase } from '../lib/types'
import { lifecycleItemsInMore, sessionItemsInMore } from '../lib/design/agent-actions'

/*
 * The agent LIST's row menu must offer the same actions as the agent DETAIL
 * page's More menu for the same phase.
 *
 * It did not. The list rendered a static Start / Stop / Restart / Delete menu
 * that never read the agent's phase, so a Running agent was offered "Start"
 * and a crashed one was offered "Restart" — the dead button kyber#599 removed
 * from the detail page, still live one screen over. Reported from a screenshot
 * of three Running agents all offering Start.
 *
 * These assertions are written against the per-phase rules in agent-actions.ts
 * rather than against a copied list of labels, so they keep holding when the
 * rules change and fail if either surface stops consulting them.
 */

vi.mock('../hooks/useAPI', () => ({
  useAgents: vi.fn(),
  useStartAgent: vi.fn(),
  useStopAgent: vi.fn(),
  useRestartAgent: vi.fn(),
  useDeleteAgent: vi.fn(),
  useForceNeedsAuthAgent: vi.fn(),
  useRepairAgentRuntime: vi.fn(),
  useRestartAgentSession: vi.fn(),
  useCompactAgentSession: vi.fn(),
}))

import { AgentActionsMenu } from './AgentList'

function agentInPhase(phase: AgentPhase): Agent {
  return {
    id: 'test-agent',
    phase,
    machine: 'local',
    runtime: 'claude-code',
    model: 'claude-opus-5',
    resources: { cpu: '1', memory: '2Gi', disk: '4Gi' },
    status: { phase },
  } as unknown as Agent
}

async function openMenuFor(phase: AgentPhase): Promise<string[]> {
  // Some cases open the menu for several phases in one test; auto-cleanup only
  // runs between tests, so without this the second render finds two triggers.
  cleanup()
  const user = userEvent.setup()
  render(<AgentActionsMenu agent={agentInPhase(phase)} onAction={vi.fn()} />)
  await user.click(screen.getByLabelText('Agent actions'))
  return screen.getAllByRole('menuitem').map((el) => el.textContent?.trim() ?? '')
}

// The labels each kind renders. Kept here, in the test, so a silent label
// change in the menu component is caught rather than mirrored.
const LABEL: Record<string, string> = {
  start: 'Start',
  stop: 'Stop',
  restart: 'Restart pod',
  'force-needs-auth': 'Require re-auth',
  'repair-runtime': 'Repair runtime',
  'retry-startup': 'Restart pod',
  'compact-session': 'Compact session',
  'restart-session': 'Restart session',
}

const PHASES: AgentPhase[] = [
  'Running',
  'Stopped',
  'Failed',
  'MemoryExhausted',
  'DiskExhausted',
  'BrokenRuntime',
  'Starting',
  'NeedsAuth',
  'WaitingForMachine',
  'Stopping',
  'Restarting',
  'Creating',
]

describe('AgentList row menu matches the per-phase action rules', () => {
  for (const phase of PHASES) {
    it(`${phase}: offers exactly the applicable actions, plus Delete`, async () => {
      const items = await openMenuFor(phase)

      const expected = [
        ...sessionItemsInMore(phase).map((k) => LABEL[k]),
        ...lifecycleItemsInMore(phase).map((k) => LABEL[k]),
        'Delete',
      ]
      // Compared as sets: WHICH actions apply is owned by agent-actions.ts,
      // but the ORDER they render in is fixed by the menu component and is
      // deliberately not the helper's array order (BrokenRuntime returns
      // repair-then-stop, the menu renders stop-then-repair). Both pages share
      // that component, so their order agrees with each other either way.
      expect([...items].sort()).toEqual([...expected].sort())
    })
  }
})

describe('AgentList row menu — the reported bugs', () => {
  it('does not offer Start on a Running agent', async () => {
    // The screenshot: three Running agents, every one offering Start. The
    // endpoint is a no-op from Running, so the item was a dead click.
    expect(await openMenuFor('Running')).not.toContain('Start')
  })

  it('does not offer Restart pod on a crashed agent (kyber#599)', async () => {
    // EventDesiredRestarting has no transition out of Failed, so this fired
    // nothing. The detail page stopped offering it; the list had not.
    for (const phase of ['Failed', 'MemoryExhausted'] as AgentPhase[]) {
      expect(await openMenuFor(phase)).not.toContain('Restart pod')
    }
  })

  it('offers the working recovery on a crashed agent instead', async () => {
    const items = await openMenuFor('Failed')
    expect(items).toContain('Start')
    expect(items).toContain('Require re-auth')
  })

  it('offers Restart pod to a NeedsAuth agent, which used to have nothing (kyber#26)', async () => {
    expect(await openMenuFor('NeedsAuth')).toContain('Restart pod')
  })

  it('offers Repair runtime on BrokenRuntime, which the list never surfaced', async () => {
    expect(await openMenuFor('BrokenRuntime')).toContain('Repair runtime')
  })

  it('offers session actions on a Running agent, matching the detail page', async () => {
    const items = await openMenuFor('Running')
    expect(items).toContain('Compact session')
    expect(items).toContain('Restart session')
  })

  it('shows only Delete on a transitional phase, with no lifecycle items', async () => {
    // Stopping/Restarting/Creating settle on their own; offering actions there
    // invites a click that does nothing.
    expect(await openMenuFor('Stopping')).toEqual(['Delete'])
  })
})
