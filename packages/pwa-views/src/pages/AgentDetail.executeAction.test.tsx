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
const { startAgent, restartAgent, switchAgentRuntime, reauthorizeAPIKey, idleMutation, effectiveModelList, agentModels } = vi.hoisted(() => ({
  startAgent: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  restartAgent: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  switchAgentRuntime: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  reauthorizeAPIKey: { mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false },
  idleMutation: () => ({ mutate: vi.fn(), mutateAsync: vi.fn(), isPending: false }),
  effectiveModelList: {
    models: [], claudeCodeVersions: [], codexVersions: [], hermesVersions: [],
    source: 'empty' as const, isLoading: false,
  },
  agentModels: vi.fn(() => ({ data: undefined, isLoading: false, isError: false })),
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
  useSwitchAgentRuntime: () => switchAgentRuntime,
  useAgentExports: () => ({ data: [], isLoading: false }),
  useStartAgentExport: idleMutation,
  useCancelAgentExport: idleMutation,
  useExportDownloadLink: idleMutation,
  useDeleteArchive: idleMutation,
  useAgentImports: () => ({ data: [], isLoading: false }),
  useCancelAgentImport: idleMutation,
  useSetAgentModel: idleMutation,
  useAgentModels: agentModels,
  useSetAgentRuntimeVersion: idleMutation,
  useSetAgentResources: idleMutation,
  usePatchAgent: idleMutation,
  useUpdateAgentProfile: idleMutation,
  useSetSessionResume: idleMutation,
  useSetRequestReplyEnabled: idleMutation,
  useSetPublicCapabilities: idleMutation,
  useAgentSkills: () => ({ data: null, isLoading: false, isError: false }),
  useDeleteAgent: idleMutation,
  useReauthorizeAgent: idleMutation,
  useReauthorizeAPIKey: () => reauthorizeAPIKey,
  useStartCodexDeviceAuth: idleMutation,
  useTokenUsage: () => ({ data: undefined }),
  useComputeConfig: vi.fn(() => ({ data: undefined })),
}))
vi.mock('../lib/models', () => ({ useEffectiveModelList: () => effectiveModelList }))
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
    effectiveModelList.hermesVersions = []
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: needsAuthAgent,
      isLoading: false,
      error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
  })

  it('does not advertise or query model selection for an unsupported runtime', () => {
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: {
        ...needsAuthAgent,
        id: 'fixed',
        phase: 'Running',
        runtime: 'fixed-harness',
        model: '',
        runtimeContract: {
          id: 'fixed-harness', name: 'Fixed harness', contractVersion: '1.0',
          profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [],
        },
      },
      isLoading: false,
      error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
    render(
      <MemoryRouter initialEntries={['/agents/fixed/general']}>
        <Routes>
          <Route path="/agents/:name/:section" element={<AgentDetail />} />
        </Routes>
      </MemoryRouter>,
    )

    expect(screen.queryByRole('button', { name: 'Model' })).not.toBeInTheDocument()
    expect(screen.queryByText('Model')).not.toBeInTheDocument()
    expect(screen.getByText('Harness version and compute allocation.')).toBeInTheDocument()
    expect(agentModels).toHaveBeenCalledWith('fixed', false)
  })

  it('switches a running agent to a configured compatible harness', async () => {
    const user = userEvent.setup()
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: { ...needsAuthAgent, id: 'switcher', phase: 'Running', runtime: 'codex', authType: 'oauth' },
      isLoading: false, error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
    vi.mocked(useAPIModule.useComputeConfig).mockReturnValue({
      data: { runtimes: [
        { id: 'codex', name: 'Codex', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [{ id: 'oauth', name: 'Subscription', flow: 'device-code' }] },
        { id: 'claude-code', name: 'Claude Code', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: ['job-turn-hooks'], authModes: [{ id: 'oauth', name: 'Subscription', flow: 'authorization-code' }] },
      ] },
    } as ReturnType<typeof useAPIModule.useComputeConfig>)
    render(
      <MemoryRouter initialEntries={['/agents/switcher/general']}>
        <Routes><Route path="/agents/:name/:section" element={<AgentDetail />} /></Routes>
      </MemoryRouter>,
    )
    await user.click(screen.getByRole('button', { name: 'Switch harness' }))
    // The harness form is the only dialog: a generic confirm stacked on top of
    // it swallowed the click and fired with no target (MAT-85).
    expect(screen.queryByText('Switch-runtime agent?')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Confirm' })).toBeNull()
    await user.selectOptions(screen.getByLabelText('Target harness'), 'claude-code')
    await user.click(screen.getAllByRole('button', { name: 'Switch harness' }).at(-1)!)
    expect(switchAgentRuntime.mutateAsync).toHaveBeenCalledWith({ name: 'switcher', runtime: 'claude-code', authType: 'oauth' })
  })

  it('offers Switch harness from the More menu', async () => {
    const user = userEvent.setup()
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: { ...needsAuthAgent, id: 'switcher', phase: 'Running', runtime: 'codex', authType: 'oauth' },
      isLoading: false, error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
    vi.mocked(useAPIModule.useComputeConfig).mockReturnValue({
      data: { runtimes: [
        { id: 'codex', name: 'Codex', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [{ id: 'oauth', name: 'Subscription', flow: 'device-code' }] },
        { id: 'claude-code', name: 'Claude Code', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [{ id: 'oauth', name: 'Subscription', flow: 'authorization-code' }] },
      ] },
    } as ReturnType<typeof useAPIModule.useComputeConfig>)
    render(
      <MemoryRouter initialEntries={['/agents/switcher/overview']}>
        <Routes><Route path="/agents/:name/:section" element={<AgentDetail />} /></Routes>
      </MemoryRouter>,
    )
    await user.click(screen.getByRole('button', { name: 'More actions' }))
    await user.click(await screen.findByRole('menuitem', { name: 'Switch harness' }))
    expect(screen.queryByRole('button', { name: 'Confirm' })).toBeNull()
    await user.selectOptions(screen.getByLabelText('Target harness'), 'claude-code')
    await user.click(screen.getByRole('button', { name: 'Switch harness' }))
    expect(switchAgentRuntime.mutateAsync).toHaveBeenCalledWith({ name: 'switcher', runtime: 'claude-code', authType: 'oauth' })
  })

  it('switches a subscription agent to an API-key-only harness by choosing its mode', async () => {
    const user = userEvent.setup()
    switchAgentRuntime.mutateAsync.mockClear()
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: { ...needsAuthAgent, id: 'switcher', phase: 'Running', runtime: 'claude-code', authType: 'oauth' },
      isLoading: false, error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
    vi.mocked(useAPIModule.useComputeConfig).mockReturnValue({
      data: { runtimes: [
        { id: 'claude-code', name: 'Claude Code', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [{ id: 'oauth', name: 'Claude subscription', flow: 'authorization-code' }] },
        { id: 'hermes', name: 'Hermes', contractVersion: '1.0', profile: 'interactive-tmux-v1', cancellation: 'notify_only', features: [], authModes: [{ id: 'api-key', name: 'OpenRouter API key', flow: 'api-key', channels: ['telegram', 'discord', 'slack'] }] },
      ] },
    } as ReturnType<typeof useAPIModule.useComputeConfig>)
    render(
      <MemoryRouter initialEntries={['/agents/switcher/general']}>
        <Routes><Route path="/agents/:name/:section" element={<AgentDetail />} /></Routes>
      </MemoryRouter>,
    )
    await user.click(screen.getByRole('button', { name: 'Switch harness' }))
    const hermes = screen.getByRole('option', { name: 'Hermes' }) as HTMLOptionElement
    expect(hermes.disabled).toBe(false)
    await user.selectOptions(screen.getByLabelText('Target harness'), 'hermes')
    expect(screen.getByLabelText('Authentication')).toHaveValue('api-key')
    expect(screen.getByRole('option', { name: 'OpenRouter API key (telegram, discord, slack)' })).toBeInTheDocument()
    await user.click(screen.getAllByRole('button', { name: 'Switch harness' }).at(-1)!)
    expect(switchAgentRuntime.mutateAsync).toHaveBeenCalledWith({ name: 'switcher', runtime: 'hermes', authType: 'api-key' })
  })

  it('offers the target API-key authorization after a switch', async () => {
    const user = userEvent.setup()
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: { ...needsAuthAgent, runtime: 'hermes', authType: 'api-key', runtimeContract: {
        id: 'hermes', name: 'Hermes', contractVersion: '1.0', profile: 'interactive-tmux-v1',
        cancellation: 'notify_only', features: [], authModes: [{ id: 'api-key', name: 'OpenRouter API key', flow: 'api-key' }],
      } }, isLoading: false, error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
    render(
      <MemoryRouter initialEntries={['/agents/lando/overview']}>
        <Routes><Route path="/agents/:name/:section" element={<AgentDetail />} /></Routes>
      </MemoryRouter>,
    )
    await user.type(screen.getByLabelText('API key'), 'new-key')
    await user.click(screen.getByRole('button', { name: 'Save key and start' }))
    expect(reauthorizeAPIKey.mutateAsync).toHaveBeenCalledWith({ name: 'lando', apiKey: 'new-key' })
  })

  it('retries with the existing custom inference credential after a switch', async () => {
    const user = userEvent.setup()
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: { ...needsAuthAgent, runtime: 'hermes', authType: 'api-key',
        inference: { baseURL: 'https://llm.example.test/v1', api: 'openai', credentialSecret: 'endpoint-key', credentialKey: 'token' },
        runtimeContract: {
          id: 'hermes', name: 'Hermes', contractVersion: '1.0', profile: 'interactive-tmux-v1',
          cancellation: 'notify_only', features: [], authModes: [{ id: 'api-key', name: 'OpenRouter API key', flow: 'api-key' }],
        },
      }, isLoading: false, error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
    render(
      <MemoryRouter initialEntries={['/agents/lando/overview']}>
        <Routes><Route path="/agents/:name/:section" element={<AgentDetail />} /></Routes>
      </MemoryRouter>,
    )
    expect(screen.queryByLabelText('API key')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Retry with existing credential' }))
    expect(startAgent.mutate).toHaveBeenCalledWith('lando')
  })

  it('browses Hermes releases without offering an unsafe source install', async () => {
    const user = userEvent.setup()
    effectiveModelList.hermesVersions = ['0.21.1', '0.21.0']
    vi.mocked(useAPIModule.useAgent).mockReturnValue({
      data: {
        ...needsAuthAgent,
        id: 'hermes',
        phase: 'Running',
        runtime: 'hermes',
        runtimeContract: {
          id: 'hermes', name: 'Hermes', contractVersion: '1.0',
          profile: 'interactive-tmux-v1', cancellation: 'notify_only',
          legacyVersionsKey: 'hermesVersions', features: [], authModes: [],
        },
      },
      isLoading: false,
      error: null,
    } as ReturnType<typeof useAPIModule.useAgent>)
    render(
      <MemoryRouter initialEntries={['/agents/hermes/general']}>
        <Routes>
          <Route path="/agents/:name/:section" element={<AgentDetail />} />
        </Routes>
      </MemoryRouter>,
    )

    await user.click(screen.getByRole('button', { name: 'Harness' }))
    expect(screen.getByText('Browse Harness Versions')).toBeInTheDocument()
    expect(screen.getByRole('option', { name: '0.21.1 (latest)' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: '0.21.0' })).toBeInTheDocument()
    expect(screen.getByText(/source-pinned runtime/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Apply' })).not.toBeInTheDocument()
  })

  it('groups agent pages into observe, automate, and configure navigation', () => {
    render(
      <MemoryRouter initialEntries={['/agents/lando']}>
        <Routes>
          <Route path="/agents/:name" element={<AgentDetail />} />
        </Routes>
      </MemoryRouter>,
    )

    const navigation = screen.getByRole('navigation', { name: 'Section pages' })
    expect(within(navigation).getAllByRole('button').map((button) => button.textContent)).toEqual([
      'OverviewHealth and live resources',
      'ActivityConversation and tool history',
      'ShellInteractive terminal',
      'JobsScheduled prompts',
      'WebhooksExternal triggers',
      'GeneralIdentity and runtime',
      'CommsConnected channels',
      'SecretsInjected credentials',
      'A2APublished capabilities',
    ])
    expect(screen.queryByRole('tab')).not.toBeInTheDocument()
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
