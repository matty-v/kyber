import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import type { UseQueryResult } from '@tanstack/react-query'
import type { Agent, ComputeConfig, Machine, ModelInfo } from '../lib/types'

// Mock the data hooks.
vi.mock('../hooks/useAPI', () => ({
  useAgents: vi.fn(),
  useMachines: vi.fn(),
  useComputeConfig: vi.fn(),
  // kyber#378 PR-D: useEffectiveModelList composes useAvailable +
  // useComputeConfig. Mock both so the wizard's picker can source
  // models either from /available or fall back to /config.
  useAvailable: vi.fn(),
  useCreateAgent: vi.fn(),
  usePutDiscordComms: vi.fn(),
  useGitHubRepos: vi.fn(),
  useGitHubRepoExists: vi.fn(),
  // useUpgradeProgress gates the submit button on an in-flight upgrade.
  useUpdates: vi.fn(() => ({ data: undefined, isError: false })),
}))

// Partially mock react-router-dom so MemoryRouter + useSearchParams come from
// the real module while useNavigate is replaced with our spy.
const navigateMock = vi.fn()
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom')
  return { ...actual, useNavigate: () => navigateMock }
})

import * as useAPIModule from '../hooks/useAPI'
import { CreateAgent } from './CreateAgent'

const machines: Machine[] = [
  {
    id: 'razer',
    spec: { provider: 'mock', machineType: 'razer-host' } as Machine['spec'],
    status: {
      phase: 'Running',
      agentCount: 0,
      availableCapacity: { cpu: '8', memory: '16Gi' },
    } as Machine['status'],
  } as unknown as Machine,
]

const models: ModelInfo[] = [
  { id: 'claude-opus-4-7', contextWindow: 1000000 },
]

const config: ComputeConfig = { models } as ComputeConfig

function setupHooks(opts?: {
  mutateAsync?: ReturnType<typeof vi.fn>
  isPending?: boolean
  configData?: ComputeConfig
  putDiscordComms?: ReturnType<typeof vi.fn>
}) {
  const mutateAsync = opts?.mutateAsync ?? vi.fn().mockResolvedValue({})
  const putDiscordAsync = opts?.putDiscordComms ?? vi.fn().mockResolvedValue({})

  vi.mocked(useAPIModule.useAgents).mockReturnValue({
    data: [] as Agent[],
    isLoading: false,
    error: null,
  } as unknown as UseQueryResult<Agent[], Error>)

  vi.mocked(useAPIModule.useMachines).mockReturnValue({
    data: machines,
    isLoading: false,
    error: null,
  } as unknown as UseQueryResult<Machine[], Error>)

  vi.mocked(useAPIModule.useComputeConfig).mockReturnValue({
    data: opts?.configData ?? config,
    isLoading: false,
    error: null,
  } as unknown as UseQueryResult<ComputeConfig, Error>)

  // kyber#378 PR-D: default the /available mock to "cache empty" so
  // useEffectiveModelList falls through to useComputeConfig — preserves
  // the pre-PR-D test contract (every existing case wins from /config).
  vi.mocked(useAPIModule.useAvailable).mockReturnValue({
    data: { claudeCodeVersions: [], models: [] },
    isLoading: false,
    error: null,
  } as unknown as UseQueryResult<unknown, Error>)

  vi.mocked(useAPIModule.useCreateAgent).mockReturnValue({
    mutateAsync,
    isPending: opts?.isPending ?? false,
  } as unknown as ReturnType<typeof useAPIModule.useCreateAgent>)

  // kyber#664: Discord is wired after the agent exists, via the same endpoint
  // the Comms tab uses. Unused unless a test enables the Discord checkbox.
  vi.mocked(useAPIModule.usePutDiscordComms).mockReturnValue({
    mutateAsync: putDiscordAsync,
    isPending: false,
  } as unknown as ReturnType<typeof useAPIModule.usePutDiscordComms>)

  // GitHub hooks default to empty/idle so the IdentitySection's
  // existing-mode dropdown falls back to the free-text input and the
  // template-mode collision check stays disabled (no name yet).
  vi.mocked(useAPIModule.useGitHubRepos).mockReturnValue({
    data: { repos: [], templates: [] },
    isSuccess: true,
    isLoading: false,
    error: null,
  } as unknown as ReturnType<typeof useAPIModule.useGitHubRepos>)
  vi.mocked(useAPIModule.useGitHubRepoExists).mockReturnValue({
    data: undefined,
    isSuccess: false,
    isFetching: false,
    error: null,
  } as unknown as ReturnType<typeof useAPIModule.useGitHubRepoExists>)

  return { mutateAsync, putDiscordAsync }
}

function renderAt(initialEntry: string = '/agents/new') {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <CreateAgent />
    </MemoryRouter>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('CreateAgent — submit happy path (api-key)', () => {
  it('walks through all 5 steps, submits, and navigates to /agents', async () => {
    const user = userEvent.setup()
    const { mutateAsync } = setupHooks()
    renderAt()

    // Step 1 — Basics
    await user.type(screen.getByLabelText(/name/i), 'alice')
    await user.selectOptions(screen.getByLabelText(/machine/i), 'razer')
    await user.click(screen.getByRole('button', { name: /next/i }))

    // Step 2 — Resources (defaults are valid)
    await waitFor(() => expect(screen.getByLabelText(/runtime/i)).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /next/i }))

    // Step 3 — Identity (template default is valid)
    await waitFor(() => expect(screen.getByLabelText(/identity repo/i)).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /next/i }))

    // Step 4 — Auth (switch to api-key, fill the key)
    await waitFor(() => expect(screen.getByLabelText(/^Authentication$/)).toBeInTheDocument())
    await user.selectOptions(screen.getByLabelText(/^Authentication$/), 'api-key')
    await user.type(screen.getByLabelText(/anthropic api key/i), 'sk-ant-test')
    await user.click(screen.getByRole('button', { name: /next/i }))

    // Step 5 — Review → click Create Agent
    await waitFor(() => expect(screen.getByRole('button', { name: /create agent/i })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /create agent/i }))

    await waitFor(() => expect(mutateAsync).toHaveBeenCalledTimes(1))
    expect(mutateAsync).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'alice',
        machine: 'razer',
        runtime: 'claude-code',
        resources: { cpu: '1', memory: '2Gi', disk: '50Gi' },
        identity: { soulDescription: undefined },
        identityRepo: { template: 'matty-v/kyber-agent-template' },
        secrets: expect.objectContaining({
          authType: 'api-key',
          telegramEnabled: false,
          anthropicApiKey: 'sk-ant-test',
          oauthCode: undefined,
          pkceVerifier: undefined,
          pkceState: undefined,
          telegramBotToken: undefined,
        }),
      }),
    )
    expect(mutateAsync.mock.calls[0][0]).not.toHaveProperty('model')
    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith('/agents'))
  // 15 s: this test drives all 5 wizard steps via userEvent; 5 s flakes under WSL2 parallel load.
  }, 15_000)
})

describe('CreateAgent — Discord wiring (kyber#664)', () => {
  // Discord needs OAuth, so this drives the OAuth path all the way through.
  async function walkToAuthStep(user: ReturnType<typeof userEvent.setup>) {
    await user.type(screen.getByLabelText(/name/i), 'barf')
    await user.selectOptions(screen.getByLabelText(/machine/i), 'razer')
    await user.click(screen.getByRole('button', { name: /next/i }))
    await waitFor(() => expect(screen.getByLabelText(/runtime/i)).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /next/i }))
    await waitFor(() => expect(screen.getByLabelText(/identity repo/i)).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /next/i }))
    await waitFor(() => expect(screen.getByLabelText(/^Authentication$/)).toBeInTheDocument())
  }

  it('refuses to create the agent when Discord has no user allowlist', async () => {
    const user = userEvent.setup()
    const { mutateAsync, putDiscordAsync } = setupHooks()
    renderAt()
    await walkToAuthStep(user)

    await user.click(screen.getByRole('checkbox', { name: 'Discord' }))
    await user.type(screen.getByLabelText(/discord bot token/i), 'bot-tok')
    // Deliberately leave "Who can talk to it" empty.
    await user.click(screen.getByRole('button', { name: /open anthropic login/i }))
    await waitFor(() =>
      expect(screen.getByLabelText(/paste authorization code/i)).toBeInTheDocument(),
    )
    await user.type(screen.getByLabelText(/paste authorization code/i), 'code123')
    await user.click(screen.getByRole('button', { name: /next/i }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /create agent/i })).toBeInTheDocument(),
    )
    await user.click(screen.getByRole('button', { name: /create agent/i }))

    // Validation happens BEFORE creation: a bad Discord config must not leave
    // behind a real agent with a half-configured channel.
    await waitFor(() =>
      expect(screen.getByText(/at least one Discord user ID/i)).toBeInTheDocument(),
    )
    expect(mutateAsync).not.toHaveBeenCalled()
    expect(putDiscordAsync).not.toHaveBeenCalled()
  }, 20_000)

  it('creates the agent first, then wires Discord through the comms endpoint', async () => {
    const user = userEvent.setup()
    const { mutateAsync, putDiscordAsync } = setupHooks()
    renderAt()
    await walkToAuthStep(user)

    await user.click(screen.getByRole('checkbox', { name: 'Discord' }))
    await user.type(screen.getByLabelText(/discord bot token/i), 'bot-tok')
    await user.type(screen.getByLabelText(/who can talk to it/i), '123456789012345678')
    await user.click(screen.getByLabelText(/only when mentioned/i))
    await user.click(screen.getByRole('button', { name: /open anthropic login/i }))
    await waitFor(() =>
      expect(screen.getByLabelText(/paste authorization code/i)).toBeInTheDocument(),
    )
    await user.type(screen.getByLabelText(/paste authorization code/i), 'code123')
    await user.click(screen.getByRole('button', { name: /next/i }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /create agent/i })).toBeInTheDocument(),
    )
    await user.click(screen.getByRole('button', { name: /create agent/i }))

    await waitFor(() => expect(putDiscordAsync).toHaveBeenCalledTimes(1))
    expect(mutateAsync).toHaveBeenCalledTimes(1)
    expect(putDiscordAsync).toHaveBeenCalledWith({
      name: 'barf',
      body: {
        botToken: 'bot-tok',
        guildIds: [],
        channelIds: [],
        allowedUserIds: ['123456789012345678'],
        mentionOnly: true,
      },
    })
    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith('/agents'))
  }, 20_000)

  it('keeps the operator on the page when Discord fails but the agent was created', async () => {
    const user = userEvent.setup()
    const failing = vi.fn().mockRejectedValue(new Error('bad bot token'))
    const { mutateAsync } = setupHooks({ putDiscordComms: failing })
    renderAt()
    await walkToAuthStep(user)

    await user.click(screen.getByRole('checkbox', { name: 'Discord' }))
    await user.type(screen.getByLabelText(/discord bot token/i), 'bot-tok')
    await user.type(screen.getByLabelText(/who can talk to it/i), '123456789012345678')
    await user.click(screen.getByRole('button', { name: /open anthropic login/i }))
    await waitFor(() =>
      expect(screen.getByLabelText(/paste authorization code/i)).toBeInTheDocument(),
    )
    await user.type(screen.getByLabelText(/paste authorization code/i), 'code123')
    await user.click(screen.getByRole('button', { name: /next/i }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /create agent/i })).toBeInTheDocument(),
    )
    await user.click(screen.getByRole('button', { name: /create agent/i }))

    // The agent is real. Saying "create failed" would be a lie that sends the
    // operator back to make a second one.
    await waitFor(() =>
      expect(screen.getByText(/Agent created, but Discord setup failed/i)).toBeInTheDocument(),
    )
    expect(mutateAsync).toHaveBeenCalledTimes(1)
    expect(navigateMock).not.toHaveBeenCalledWith('/agents')
  }, 20_000)
})

describe('CreateAgent — OAuth state-mismatch error', () => {
  it('blocks submit when pasted state does not match pkceState', async () => {
    const user = userEvent.setup()
    const { mutateAsync } = setupHooks()
    const randomUUID = vi.spyOn(crypto, 'randomUUID').mockReturnValue('expected-state' as `${string}-${string}-${string}-${string}-${string}`)
    const open = vi.spyOn(window, 'open').mockReturnValue(null)

    try {
      renderAt()

      // Steps 1-3 same as happy path
      await user.type(screen.getByLabelText(/name/i), 'alice')
      await user.selectOptions(screen.getByLabelText(/machine/i), 'razer')
      await user.click(screen.getByRole('button', { name: /next/i }))
      await user.click(screen.getByRole('button', { name: /next/i })) // Resources
      await user.click(screen.getByRole('button', { name: /next/i })) // Identity

      // Step 4 — OAuth: click Authorize, paste a wrong-state code
      await waitFor(() => expect(screen.getByLabelText(/^Authentication$/)).toBeInTheDocument())
      await user.click(screen.getByRole('button', { name: /open anthropic login/i }))
      await waitFor(() =>
        expect(screen.getByLabelText(/paste authorization code/i)).toBeInTheDocument(),
      )
      await user.type(screen.getByLabelText(/paste authorization code/i), 'real-code#wrong-state')
      await user.click(screen.getByRole('button', { name: /next/i }))

      // Step 5 — click Create Agent, expect error not submission
      await waitFor(() =>
        expect(screen.getByRole('button', { name: /create agent/i })).toBeInTheDocument(),
      )
      await user.click(screen.getByRole('button', { name: /create agent/i }))

      await waitFor(() =>
        expect(screen.getByText(/state mismatch/i)).toBeInTheDocument(),
      )
      expect(mutateAsync).not.toHaveBeenCalled()
      expect(navigateMock).not.toHaveBeenCalled()
    } finally {
      randomUUID.mockRestore()
      open.mockRestore()
    }
  })
})

describe('CreateAgent — deep-link guard', () => {
  it('bounces ?step=4 to ?step=1 on a fresh empty state', async () => {
    setupHooks()
    renderAt('/agents/new?step=4')

    // Render-time clamping lands user on Basics (step 1) regardless of URL.
    await waitFor(() => {
      expect(screen.getByLabelText(/name/i)).toBeInTheDocument()
    })
  })
})

describe('CreateAgent — fleet defaults', () => {
  it('shows fleet defaults in Review instead of pinning a model', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    // Drive forward to step 5 to inspect the inherited settings summary.
    await user.type(screen.getByLabelText(/name/i), 'alice')
    await user.selectOptions(screen.getByLabelText(/machine/i), 'razer')
    await user.click(screen.getByRole('button', { name: /next/i }))
    await user.click(screen.getByRole('button', { name: /next/i })) // Resources
    await user.click(screen.getByRole('button', { name: /next/i })) // Identity
    await user.selectOptions(screen.getByLabelText(/^Authentication$/), 'api-key')
    await user.type(screen.getByLabelText(/anthropic api key/i), 'sk-ant-x')
    await user.click(screen.getByRole('button', { name: /next/i }))

    // Both model and harness version inherit their runtime-scoped defaults.
    await waitFor(() => expect(screen.getAllByText('Fleet default')).toHaveLength(2))
  })
})

describe('CreateAgent — Cancel + discard dialog', () => {
  it('clean state: Cancel navigates immediately, no dialog', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    await user.click(screen.getByRole('button', { name: /cancel/i }))
    expect(navigateMock).toHaveBeenCalledWith('/agents')
    // No dialog opened.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('dirty state: Cancel opens the discard dialog and does NOT navigate', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    // Dirty the state by typing into the Name field.
    await user.type(screen.getByLabelText(/name/i), 'alice')
    await user.click(screen.getByRole('button', { name: /cancel/i }))

    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(screen.getByText(/discard changes/i)).toBeInTheDocument()
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('dirty state: dialog Cancel closes the dialog and keeps state', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    await user.type(screen.getByLabelText(/name/i), 'alice')
    await user.click(screen.getByRole('button', { name: /cancel/i }))
    // Dialog is open — click its Cancel button (NOT the form's).
    const dialog = screen.getByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /cancel/i }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    // Form state preserved.
    expect((screen.getByLabelText(/name/i) as HTMLInputElement).value).toBe('alice')
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('dirty state: dialog Discard navigates to /agents', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    await user.type(screen.getByLabelText(/name/i), 'alice')
    await user.click(screen.getByRole('button', { name: /cancel/i }))
    const dialog = screen.getByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /discard/i }))

    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith('/agents'))
  })

  it('dirty state: pressing Esc opens the dialog (same path as Cancel button)', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    await user.type(screen.getByLabelText(/name/i), 'alice')
    await user.keyboard('{Escape}')

    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument())
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('dirty state: full Esc → dialog → Discard chain navigates to /agents', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    await user.type(screen.getByLabelText(/name/i), 'alice')
    await user.keyboard('{Escape}')

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /discard/i }))

    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith('/agents'))
  })
})

describe('CreateAgent — Enter-advances keyboard flow', () => {
  it('Enter from a focused input advances to the next step', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    // Step 1 is Basics; Name + Machine are required for the validator to pass.
    // Set Machine first so focus lands back on the Name input afterward.
    await user.selectOptions(screen.getByLabelText(/machine/i), 'razer')
    await user.type(screen.getByLabelText(/name/i), 'enter-alice')

    // Sanity: still on Basics — Resources fields aren't rendered yet.
    expect(screen.queryByLabelText(/runtime/i)).not.toBeInTheDocument()

    // Focus is on the Name input (last user.type target). The hook's
    // input-tag check lets Enter through; the orchestrator's onEnter
    // advances the active step.
    await user.keyboard('{Enter}')

    await waitFor(() => expect(screen.getByLabelText(/runtime/i)).toBeInTheDocument())
  })

  it('Enter from a focused input does not submit when the current step is invalid', async () => {
    const user = userEvent.setup()
    const { mutateAsync } = setupHooks()
    renderAt()

    // Type a name but leave Machine empty — Basics validator should fail.
    const nameInput = screen.getByLabelText(/name/i)
    await user.type(nameInput, 'incomplete-alice')
    await user.keyboard('{Enter}')

    // Still on Basics; Resources fields not rendered.
    expect(screen.queryByLabelText(/runtime/i)).not.toBeInTheDocument()
    // No agent-create attempt happened.
    expect(mutateAsync).not.toHaveBeenCalled()
  })
})

describe('CreateAgent — per-step validation reason', () => {
  it('renders the validator reason inline when the current step is invalid', () => {
    setupHooks()
    renderAt()

    // Fresh state: Basics is invalid because Name + Machine are empty. The
    // validator returns the Name-first reason (empty Name comes first).
    expect(screen.getByTestId('wizard-step-reason')).toHaveTextContent(
      'Pick a name for the agent.',
    )
  })

  it('updates the reason as the user fills required fields in order', async () => {
    const user = userEvent.setup()
    setupHooks()
    renderAt()

    // After typing a name, the reason advances to "Pick a machine."
    await user.type(screen.getByLabelText(/name/i), 'alice')
    expect(screen.getByTestId('wizard-step-reason')).toHaveTextContent('Pick a machine.')

    // After picking a machine, the step is valid and the reason disappears.
    await user.selectOptions(screen.getByLabelText(/machine/i), 'razer')
    expect(screen.queryByTestId('wizard-step-reason')).not.toBeInTheDocument()
  })
})
