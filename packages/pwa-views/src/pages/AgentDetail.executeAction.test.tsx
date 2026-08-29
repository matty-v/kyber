import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import type { Agent } from '../lib/types'

// kyber#26, second review round. The kind→endpoint mapping lives in a tested
// helper (lifecycleActionEndpoint), but Chewie's re-review pointed out the fix
// was half a fix: nothing pinned the seam that CONSUMES it. Replacing
// `isLifecycleKind(pending) ? lifecycleActionEndpoint(pending) : pending` with a
// bare `pending` left 670/670 vitest green, tsc clean and lint clean — and
// restored the dead button the whole issue exists to prevent, because
// 'retry-startup' is not an endpoint any mutation answers to.
//
// This mounts the real page and drives the real click path, so that edit reds.

// vi.hoisted: vi.mock's factory is lifted above the imports, so anything it
// closes over has to be hoisted with it.
const { startAgent, restartAgent, idleMutation } = vi.hoisted(() => ({
  startAgent: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  restartAgent: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  idleMutation: () => ({ mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false }),
}))

vi.mock('../hooks/useAPI', () => ({
  useAgent: vi.fn(),
  useStartAgent: () => startAgent,
  useRestartAgent: () => restartAgent,
  useStopAgent: idleMutation,
  useRestartAgentSession: idleMutation,
  useCompactAgentSession: idleMutation,
  useForceNeedsAuthAgent: idleMutation,
  useRepairAgentRuntime: idleMutation,
  useSetAgentModel: idleMutation,
  useAgentModels: () => ({ data: undefined, isLoading: false, isError: false }),
  useSetAgentRuntimeVersion: idleMutation,
  useSetAgentResources: idleMutation,
  usePatchAgent: idleMutation,
  useSetSessionResume: idleMutation,
  useSetRequestReplyEnabled: idleMutation,
  useDeleteAgent: idleMutation,
  useReauthorizeAgent: idleMutation,
  useStartCodexDeviceAuth: idleMutation,
  useTokenUsage: () => ({ data: undefined }),
  useComputeConfig: () => ({ data: undefined }),
}))
vi.mock('../lib/models', () => ({ useEffectiveModelList: () => ({ data: undefined }) }))
vi.mock('../components/TerminalPeek', () => ({
  AgentTerminalPeek: ({ agentName, hasPod }: { agentName: string; hasPod: boolean }) => (
    <div data-testid="agent-terminal-peek" data-agent-name={agentName} data-has-pod={String(hasPod)} />
  ),
}))

import * as useAPIModule from '../hooks/useAPI'
import { AgentDetail } from './AgentDetail'

// Radix menus/dialogs lean on a few browser APIs jsdom doesn't implement.
if (typeof Element !== 'undefined') {
  if (typeof Element.prototype.scrollIntoView !== 'function')
    Element.prototype.scrollIntoView = function () {}
  if (typeof Element.prototype.hasPointerCapture !== 'function')
    Element.prototype.hasPointerCapture = function () {
      return false
    }
  if (typeof Element.prototype.releasePointerCapture !== 'function')
    Element.prototype.releasePointerCapture = function () {}
}
if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
}

const needsAuthAgent: Agent = {
  id: 'lando',
  phase: 'NeedsAuth',
  machine: 'falcon',
  runtime: 'claude-code',
  model: 'claude-opus-5',
  resources: { cpu: '1', memory: '1Gi', disk: '10Gi' },
  status: {} as Agent['status'],
  createdAt: '2026-08-09T11:37:00Z',
}

describe('AgentDetail executeAction — NeedsAuth Restart pod (kyber#26)', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: needsAuthAgent,
      isLoading: false,
      error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
  })

  it('confirming Restart pod calls the START mutation, never the restart one', async () => {
    const user = userEvent.setup()
    // Mounted under a real route so useParams supplies the agent name — the
    // page reads it from there and passes it to the mutation.
    render(
      <MemoryRouter initialEntries={['/agents/lando']}>
        <Routes>
          <Route path="/agents/:name" element={<AgentDetail />} />
        </Routes>
      </MemoryRouter>,
    )

    expect(screen.getByTestId('agent-terminal-peek')).toHaveAttribute('data-agent-name', 'lando')
    expect(screen.getByTestId('agent-terminal-peek')).toHaveAttribute('data-has-pod', 'false')

    await user.click(screen.getByRole('button', { name: /More actions/i }))
    await user.click(await screen.findByRole('menuitem', { name: /Restart pod/ }))

    // The confirm gate stands between the click and the call — drive it, so the
    // test exercises the same path an operator does.
    const dialog = await screen.findByRole('dialog')
    const confirm = within(dialog).getByRole('button', { name: /^(Confirm|Restart pod|Continue|Yes)/i })
    await user.click(confirm)

    // The assertion Chewie asked for: the seam resolves 'retry-startup' to the
    // start endpoint. `const action = pending` reds here, because nothing
    // matches the literal 'retry-startup'.
    expect(startAgent.mutateAsync).toHaveBeenCalledWith('lando')
    expect(restartAgent.mutateAsync).not.toHaveBeenCalled()
  })
})
